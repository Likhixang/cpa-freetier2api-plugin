package core

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestTrimToBytesStopsAtRuneBoundary 覆盖中文正文被按字节截断的场景：
// 切点必须回退到字符起始字节，否则日志与面板里会出现 U+FFFD。
func TestTrimToBytesStopsAtRuneBoundary(t *testing.T) {
	cases := []struct {
		name  string
		value string
		limit int
		want  string
	}{
		{"短于上限原样返回", "abc", 10, "abc"},
		{"上限为零返回空", "abc", 0, ""},
		{"上限为负返回空", "abc", -1, ""},
		{"正好落在字符边界", "你好", 3, "你"},
		{"落在续字节上回退一个字符", "你好", 4, "你"},
		{"落在续字节上回退一个字符（5 字节）", "你好", 5, "你"},
		{"两个完整字符", "你好", 6, "你好"},
		{"ASCII 按字节切", "abcdef", 4, "abcd"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := TrimToBytes(testCase.value, testCase.limit); got != testCase.want {
				t.Fatalf("TrimToBytes(%q, %d) = %q, want %q", testCase.value, testCase.limit, got, testCase.want)
			}
		})
	}
}

// TestTruncateKeepsRunesIntact 确认 Truncate 不再把字符劈成半个。
func TestTruncateKeepsRunesIntact(t *testing.T) {
	got := Truncate("上游返回了一段很长的中文错误说明", 12)
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("截断结果出现替换字符：%q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("截断结果应带省略号：%q", got)
	}
}

// TestFailureHTTPStatusPolicy 覆盖两条状态码策略：
// 账号级故障一律 403（否则宿主错过软冷却分支），2xx 状态码一律按分类推导。
func TestFailureHTTPStatusPolicy(t *testing.T) {
	cases := []struct {
		name string
		err  *Failure
		want int
	}{
		{"账号故障包在 400 里也要按 403 上报", &Failure{Kind: FailureKindAccountFault, Status: http.StatusBadRequest}, http.StatusForbidden},
		{"会话失效保留上游 403", &Failure{Kind: FailureKindSessionDead, Status: http.StatusForbidden}, http.StatusForbidden},
		{"上游 200 包体里的错误按分类推导", &Failure{Kind: FailureKindTransient, Status: http.StatusOK}, http.StatusBadGateway},
		{"429 原样透传", &Failure{Kind: FailureKindRateLimit, Status: http.StatusTooManyRequests}, http.StatusTooManyRequests},
		{"402 原样透传", &Failure{Kind: FailureKindHardCredit, Status: http.StatusPaymentRequired}, http.StatusPaymentRequired},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.err.HTTPStatus(); got != testCase.want {
				t.Fatalf("HTTPStatus() = %d, want %d", got, testCase.want)
			}
		})
	}
}

// TestToPluginErrorRestoresBridgeStatus 覆盖宿主 HTTP 桥把响应折叠成错误文本的
// 场景：状态码必须按文本还原，否则 429 会被当成 502，宿主不做限流冷却。
func TestToPluginErrorRestoresBridgeStatus(t *testing.T) {
	raw := errors.New(`host.http.do_stream: host_call_failed: execute host http request: ` +
		`Post "https://www.workbuddy.ai/v2/chat/completions": Too Many Requests`)

	pluginErr, okPlugin := ToPluginError(raw).(*PluginError)
	if !okPlugin {
		t.Fatal("应翻译成 *PluginError")
	}
	if pluginErr.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatus = %d, want 429", pluginErr.HTTPStatus)
	}
	if !pluginErr.Retryable {
		t.Fatal("429 应标记为可重试")
	}
	if pluginErr.Code != "upstream_soft_rate" {
		t.Fatalf("Code = %q, want upstream_soft_rate", pluginErr.Code)
	}
	if !strings.Contains(pluginErr.Message, "上游 HTTP 429") {
		t.Fatalf("消息未还原成可读形态：%q", pluginErr.Message)
	}
	if !strings.Contains(pluginErr.Message, "www.workbuddy.ai") {
		t.Fatalf("消息应带上上游主机便于定位：%q", pluginErr.Message)
	}
}

// TestToPluginErrorLeavesPlainErrorsAlone 确认普通错误里的 "not found" 字样
// 不会被误判成上游 404。
func TestToPluginErrorLeavesPlainErrorsAlone(t *testing.T) {
	pluginErr, okPlugin := ToPluginError(errors.New("open /etc/x.json: file not found")).(*PluginError)
	if !okPlugin {
		t.Fatal("应翻译成 *PluginError")
	}
	if pluginErr.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("普通错误应按 502 处理，实际 %d", pluginErr.HTTPStatus)
	}
}

// TestRetryAfterPropagation 覆盖上游明示等待时间的透传：插件内部重试与宿主
// 的 Retry-After 必须读同一个值。
func TestRetryAfterPropagation(t *testing.T) {
	failure := &Failure{Kind: FailureKindSoftRate, Status: http.StatusTooManyRequests, Msg: "slow down", RetryAfter: 2500 * time.Millisecond}
	if got := RetryAfterOf(failure); got != 2500*time.Millisecond {
		t.Fatalf("RetryAfterOf = %v, want 2.5s", got)
	}
	pluginErr, okPlugin := ToPluginError(failure).(*PluginError)
	if !okPlugin {
		t.Fatal("应翻译成 *PluginError")
	}
	if pluginErr.RetryAfterSeconds != 3 {
		t.Fatalf("RetryAfterSeconds = %d, want 3（向上取整）", pluginErr.RetryAfterSeconds)
	}
	if got := RetryAfterOf(errors.New("plain")); got != 0 {
		t.Fatalf("无提示的错误应返回 0，实际 %v", got)
	}
}

// TestPluginErrorMessageKeepsUpstreamDetail 确认分类错误的消息优先给上游原文。
func TestPluginErrorMessageKeepsUpstreamDetail(t *testing.T) {
	failure := &Failure{Kind: FailureKindAccountFault, Status: http.StatusOK, Msg: "账号已被限制使用"}
	pluginErr, okPlugin := ToPluginError(failure).(*PluginError)
	if !okPlugin {
		t.Fatal("应翻译成 *PluginError")
	}
	if !strings.Contains(pluginErr.Message, "账号已被限制使用") {
		t.Fatalf("消息应保留上游原文：%q", pluginErr.Message)
	}
	if pluginErr.HTTPStatus != http.StatusForbidden {
		t.Fatalf("HTTPStatus = %d, want 403", pluginErr.HTTPStatus)
	}
}

// TestErrorEnvelopeCarriesRetryAfter 确认信封里带 retry_after_seconds 字段。
func TestErrorEnvelopeCarriesRetryAfter(t *testing.T) {
	raw := ErrorEnvelopeFromError(&PluginError{Code: "upstream_soft_rate", Message: "slow down", Retryable: true, HTTPStatus: 429, RetryAfterSeconds: 7})
	if !strings.Contains(string(raw), `"retry_after_seconds":7`) {
		t.Fatalf("信封未携带 retry_after_seconds：%s", raw)
	}
}
