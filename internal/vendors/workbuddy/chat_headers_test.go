package workbuddy

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
)

// uuidV7HexRe 是官方客户端对 conversationRequestId 的硬校验正则
// （bundle 偏移 ~7309100）：小写 UUIDv7 的 32 位十六进制形态。
var uuidV7HexRe = regexp.MustCompile(`^[0-9a-f]{12}7[0-9a-f]{3}[89ab][0-9a-f]{15}$`)

// uuidV7DashedRe 是带连字符标准 UUIDv7 的校验正则。
var uuidV7DashedRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestNewUUIDV7Format 验证生成的会话 ID 是合法的标准 UUIDv7（含连字符与去横线两款）。
func TestNewUUIDV7Format(t *testing.T) {
	seenHex := map[string]bool{}
	seenDashed := map[string]bool{}
	for i := 0; i < 500; i++ {
		idHex := NewUUIDv7Hex()
		if len(idHex) != 32 || !uuidV7HexRe.MatchString(idHex) {
			t.Fatalf("NewUUIDv7Hex = %q, 不符合 UUIDv7 32hex 格式", idHex)
		}
		if seenHex[idHex] {
			t.Fatalf("duplicate hex id %q", idHex)
		}
		seenHex[idHex] = true

		idDashed := NewUUIDv7()
		if len(idDashed) != 36 || !uuidV7DashedRe.MatchString(idDashed) {
			t.Fatalf("NewUUIDv7 = %q, 不符合 UUIDv7 dashed 格式", idDashed)
		}
		if seenDashed[idDashed] {
			t.Fatalf("duplicate dashed id %q", idDashed)
		}
		seenDashed[idDashed] = true
	}
}

// TestChatHeadersMatchGenuineClientCN 验证国内版对话请求头与正版 CLI 直发形态一致：
// 版本三段 UA (5.11.3)、归属头（X-Product=SaaS 等）、UUIDv7 会话头族、W3C trace 族、Origin/Referer/语言。
func TestChatHeadersMatchGenuineClientCN(t *testing.T) {
	rt := newRuntime(core.Env{})
	rt.configure(core.Settings{MachineSalt: "salt-1"}, map[string]string{})
	cred := &Credential{AccessToken: "token", UID: "a707ecea-6211-4e4e-a1b4-13335f8ed5bc", realm: RegionCN}
	c := NewClient(rt, Options{})

	req, errReq := http.NewRequest(http.MethodPost, GetEndpoints(RegionCN).ChatBase+chatCompletionsPath, nil)
	if errReq != nil {
		t.Fatal(errReq)
	}
	meta := ChatMetaFromBody([]byte(`{"model":"kimi-k3.1-pro","messages":[{"role":"user","content":"hi"}]}`))
	c.applyChatHeaders(req, cred, meta, "")

	// 归属与版本。
	ua := req.Header.Get("User-Agent")
	if ua != "WorkBuddy/5.11.3 WorkBuddy/5.11.3 CLI/2.179.0" {
		t.Fatalf("User-Agent = %q", ua)
	}
	for key, want := range map[string]string{
		"X-Product":           "SaaS",
		"X-IDE-Type":          "WorkBuddy",
		"X-IDE-Name":          "WorkBuddy",
		"X-IDE-Version":       "5.11.3",
		"X-Agent-Intent":      "craft",
		"X-Agent-Purpose":     "conversation",
		"X-Agent-Type":        "main",
		"X-Private-Data":      "false",
		"X-User-Id":           cred.UID,
		"X-Domain":            "www.workbuddy.cn",
		"X-CodeBuddy-Request": "1",
		"Origin":              "https://www.workbuddy.cn",
		"Referer":             "https://www.workbuddy.cn/",
		"Accept-Language":     "zh-CN",
	} {
		if got := req.Header.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}

	// 会话头族：X-Conversation-ID 为带连字符 UUID，X-Root-Request-ID 等于 X-Conversation-Request-ID。
	convID := req.Header.Get("X-Conversation-ID")
	if len(convID) != 36 || strings.Count(convID, "-") != 4 || !uuidV7DashedRe.MatchString(convID) {
		t.Fatalf("X-Conversation-ID = %q, 必须为标准带连字符 UUID", convID)
	}
	convReqID := req.Header.Get("X-Conversation-Request-ID")
	rootReqID := req.Header.Get("X-Root-Request-ID")
	if convReqID == "" || !uuidV7HexRe.MatchString(convReqID) {
		t.Fatalf("X-Conversation-Request-ID = %q 非法", convReqID)
	}
	if rootReqID != convReqID {
		t.Fatalf("X-Root-Request-ID = %q 必须等于 X-Conversation-Request-ID = %q", rootReqID, convReqID)
	}
	msgID := req.Header.Get("X-Conversation-Message-ID")
	reqID := req.Header.Get("X-Request-ID")
	if msgID == "" || !uuidV7HexRe.MatchString(msgID) {
		t.Fatalf("X-Conversation-Message-ID = %q 非法", msgID)
	}
	if reqID != msgID {
		t.Fatalf("X-Request-ID = %q 必须等于 X-Conversation-Message-ID = %q", reqID, msgID)
	}

	// trace 族：b3 为三段式 traceId-spanId-1（不伪造 ParentSpanId）。
	traceID := req.Header.Get("X-Trace-ID")
	spanID := req.Header.Get("X-B3-SpanId")
	if len(traceID) != 32 || len(spanID) != 16 {
		t.Fatalf("trace id lengths = %d/%d", len(traceID), len(spanID))
	}
	if req.Header.Get("traceparent") != "00-"+traceID+"-"+spanID+"-01" {
		t.Fatalf("traceparent = %q", req.Header.Get("traceparent"))
	}
	if req.Header.Get("b3") != traceID+"-"+spanID+"-1" {
		t.Fatalf("b3 = %q", req.Header.Get("b3"))
	}
	if req.Header.Get("X-B3-TraceId") != traceID || req.Header.Get("X-B3-Sampled") != "1" {
		t.Fatal("X-B3-TraceId/X-B3-Sampled mismatch")
	}
	if traceID == req.Header.Get("X-Conversation-Request-ID") {
		t.Fatal("trace id must not reuse the conversation request id")
	}

	// 对话请求不发送的头。
	for _, key := range []string{
		"X-Device-Token", "X-Machine-ID", "X-Session-ID", "X-B3-ParentSpanId",
	} {
		if got := req.Header.Get(key); got != "" {
			t.Fatalf("%s must not be sent on chat, got %q", key, got)
		}
	}
	if !strings.Contains(req.Header.Get("Accept"), "text/event-stream") {
		t.Fatalf("Accept = %q", req.Header.Get("Accept"))
	}

	// 第二次构造：trace 族与轮 ID 都是新的。
	second, errSecond := http.NewRequest(http.MethodPost, req.URL.String(), nil)
	if errSecond != nil {
		t.Fatal(errSecond)
	}
	c.applyChatHeaders(second, cred, ChatMetaFromBody([]byte(`{"model":"kimi-k3.1-pro"}`)), "")
	if second.Header.Get("X-Trace-ID") == traceID {
		t.Fatal("trace id must be regenerated per request")
	}
}

// TestChatHeadersMatchGenuineClientGlobal 验证国际版对话请求头：
// UA (5.11.4 workbuddy-ai CLI/2.161.0)、X-Private-Data=true、X-Domain=www.workbuddy.ai、完整会话头族。
func TestChatHeadersMatchGenuineClientGlobal(t *testing.T) {
	rt := newRuntime(core.Env{})
	rt.configure(core.Settings{MachineSalt: "salt-1"}, map[string]string{})
	cred := &Credential{AccessToken: "token-global", UID: "110693a0-d32d-4d81-8a0a-7a3445069f96", realm: RegionGlobal}
	c := NewClient(rt, Options{})

	req, errReq := http.NewRequest(http.MethodPost, GetEndpoints(RegionGlobal).ChatBase+chatCompletionsPath, nil)
	if errReq != nil {
		t.Fatal(errReq)
	}
	meta := ChatMetaFromBody([]byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`))
	c.applyChatHeaders(req, cred, meta, "")

	// 国际版 5.11.4 身份核对。
	ua := req.Header.Get("User-Agent")
	if ua != "WorkBuddy/5.11.4 workbuddy-ai/5.11.4 CLI/2.161.0" {
		t.Fatalf("Global User-Agent = %q, want WorkBuddy/5.11.4 workbuddy-ai/5.11.4 CLI/2.161.0", ua)
	}
	for key, want := range map[string]string{
		"X-Product":           "SaaS",
		"X-IDE-Type":          "WorkBuddy",
		"X-IDE-Name":          "WorkBuddy",
		"X-IDE-Version":       "5.11.4",
		"X-Agent-Intent":      "craft",
		"X-Agent-Purpose":     "conversation",
		"X-Agent-Type":        "main",
		"X-Private-Data":      "true",
		"X-User-Id":           cred.UID,
		"X-Domain":            "www.workbuddy.ai",
		"X-CodeBuddy-Request": "1",
		"Origin":              "https://www.workbuddy.ai",
		"Referer":             "https://www.workbuddy.ai/",
		"Accept-Language":     "en-US",
	} {
		if got := req.Header.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}

	// 国际版不发 X-No-Enterprise-Id。
	if got := req.Header.Get("X-No-Enterprise-Id"); got != "" {
		t.Fatalf("X-No-Enterprise-Id = %q, global should not send it", got)
	}

	// 会话头族必须齐全（11128 渠道校验核心判据）。
	convID := req.Header.Get("X-Conversation-ID")
	if len(convID) != 36 || strings.Count(convID, "-") != 4 || !uuidV7DashedRe.MatchString(convID) {
		t.Fatalf("Global X-Conversation-ID = %q, 必须为标准带连字符 UUID", convID)
	}
	convReqID := req.Header.Get("X-Conversation-Request-ID")
	rootReqID := req.Header.Get("X-Root-Request-ID")
	if convReqID == "" || !uuidV7HexRe.MatchString(convReqID) {
		t.Fatalf("Global X-Conversation-Request-ID = %q", convReqID)
	}
	if rootReqID != convReqID {
		t.Fatalf("Global X-Root-Request-ID = %q must match %q", rootReqID, convReqID)
	}
}
