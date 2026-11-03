package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 本文件定义供应商错误的中立契约。
//
// 背景：插件把「上游错误」翻译成宿主认得的 HTTP 状态码，宿主据此决定
// 冷却/禁用/轮转。此前根层只认 workbuddy.Error，其它供应商的错误全部退化成
// 502 retryable（401 不会让凭证失效、429 不会触发限流处置）。现在每个供应商
// 的分类错误都实现本接口，由 ToPluginError 统一翻译。

// UpstreamError 是供应商分类后的上游错误。
//
// 方法名刻意避开 Kind / Status / Message：各家的 Error 结构体已经用了这些
// 字段名，而同名字段与方法在 Go 里不能共存。
type UpstreamError interface {
	error
	// HTTPStatus 返回映射给宿主的 HTTP 状态码。
	HTTPStatus() int
	// Retryable 报告宿主是否应该换一个凭证重试同一请求。
	Retryable() bool
	// UpstreamKind 返回错误分类标识（用于日志与错误码）。
	UpstreamKind() string
}

// RetryHinter 由「上游明确告诉你要等多久」的错误实现。
//
// 宿主会把等待秒数翻成 Retry-After 响应头交给客户端；没实现或返回 0 时
// 保持原样（不额外发头）。插件内部的重试也读同一个值，避免两套等待口径。
type RetryHinter interface {
	RetryAfter() time.Duration
}

// RetryAfterOf 取上游明示的等待时长。
//
// 先看 RetryHinter 接口，再回落到 *Failure 的 RetryAfter 字段：插件里绝大多数
// 分类错误都是 *Failure，字段比方法更自然（也不会和既有字段名冲突）。
func RetryAfterOf(err error) time.Duration {
	if err == nil {
		return 0
	}
	var hinter RetryHinter
	if errors.As(err, &hinter) {
		if wait := hinter.RetryAfter(); wait > 0 {
			return wait
		}
	}
	var failure *Failure
	if errors.As(err, &failure) && failure.RetryAfter > 0 {
		return failure.RetryAfter
	}
	return 0
}

// UpstreamDetail 由能提供上游原始响应正文的错误实现。
//
// 用途：流式错误帧与日志里优先给用户看上游原文（诊断价值更高），
// 而不是插件重写的分类描述。
type UpstreamDetail interface {
	error
	UpstreamMessage() string
}

// ToPluginError 把供应商错误翻译成宿主契约错误。
//
// 非分类错误按 502 可重试处理：那通常是本地问题（编解码、连接），
// 换号重试是安全的。
func ToPluginError(err error) error {
	if err == nil {
		return nil
	}
	var upstreamErr UpstreamError
	if errors.As(err, &upstreamErr) {
		status := upstreamErr.HTTPStatus()
		if status <= 0 {
			status = http.StatusBadGateway
		}
		code := upstreamCode(upstreamErr.UpstreamKind())
		pluginErr := NewPluginError(code, err.Error(), status)
		if upstreamErr.Retryable() {
			pluginErr.Retryable = true
		}
		pluginErr.RetryAfterSeconds = retryAfterSeconds(err)
		return pluginErr
	}
	// 宿主 HTTP 桥在非 2xx 时会把整个响应折叠成一条 RPC 错误文本，插件拿不到
	// http.Response。这类错误必须按文本还原状态码：否则 429 会被当成普通传输
	// 故障报 502，宿主不做限流冷却，客户端拿到的是 500。
	if failure := reconstructBridgeFailure(err); failure != nil {
		status := failure.HTTPStatus()
		code := upstreamCode(failure.Kind.String())
		pluginErr := NewPluginError(code, failure.Msg, status)
		if failure.Retryable() {
			pluginErr.Retryable = true
		}
		pluginErr.RetryAfterSeconds = retryAfterSeconds(failure)
		return pluginErr
	}
	return RetryablePluginError("upstream_request_failed", err.Error(), http.StatusBadGateway)
}

// bridgeStatusMarkers 是宿主桥错误文本里的状态短语到状态码的映射。
//
// 顺序敏感：长短语在前，"Request Entity Too Large" 不能被更短的短语先吃掉。
var bridgeStatusMarkers = []struct {
	marker string
	status int
}{
	{"Request Entity Too Large", http.StatusRequestEntityTooLarge},
	{"Service Unavailable", http.StatusServiceUnavailable},
	{"Gateway Timeout", http.StatusGatewayTimeout},
	{"Too Many Requests", http.StatusTooManyRequests},
	{"Payment Required", http.StatusPaymentRequired},
	{"Unauthorized", http.StatusUnauthorized},
	{"Bad Gateway", http.StatusBadGateway},
	{"Forbidden", http.StatusForbidden},
	{"Not Found", http.StatusNotFound},
	{"Bad Request", http.StatusBadRequest},
}

// reconstructBridgeFailure 把宿主 HTTP 桥的传输层错误还原成分类错误。
//
// 只处理带桥标记的错误：普通错误里出现 "not found"（例如本地文件缺失）不该
// 被误判成上游 404。返回的 Msg 剥掉内部前缀，只留「上游 HTTP 429 Too Many
// Requests（www.workbuddy.ai）」这种可读形态。
func reconstructBridgeFailure(err error) *Failure {
	if err == nil {
		return nil
	}
	message := err.Error()
	if !strings.Contains(message, "host.http.do") && !strings.Contains(message, "host_call_failed") {
		return nil
	}
	status := 0
	for _, marker := range bridgeStatusMarkers {
		if strings.Contains(message, marker.marker) {
			status = marker.status
			break
		}
	}
	if status == 0 {
		return nil
	}
	return &Failure{Kind: bridgeFailureKind(status), Status: status, Msg: bridgeFailureMessage(status, message)}
}

// bridgeFailureKind 把还原出的状态码映射成可处置分类。
func bridgeFailureKind(status int) FailureKind {
	switch status {
	case http.StatusTooManyRequests:
		return FailureKindSoftRate
	case http.StatusUnauthorized:
		return FailureKindCredential
	case http.StatusPaymentRequired:
		return FailureKindHardCredit
	case http.StatusForbidden:
		return FailureKindWAFBlock
	case http.StatusNotFound:
		return FailureKindNotFound
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return FailureKindBadParams
	case http.StatusServiceUnavailable:
		return FailureKindBusy
	default:
		return FailureKindTransient
	}
}

// bridgeFailureMessage 生成给下游看的上游错误描述，附上被请求的主机便于定位。
func bridgeFailureMessage(status int, raw string) string {
	message := fmt.Sprintf("上游 HTTP %d %s", status, http.StatusText(status))
	if host := bridgeTargetHost(raw); host != "" {
		return fmt.Sprintf("%s（%s）", message, host)
	}
	return message
}

// bridgeTargetHost 从 Go http.Client 的错误文本里取出上游主机名。
//
// 典型形态：Post "https://www.workbuddy.ai/v2/chat/completions": Too Many Requests
func bridgeTargetHost(raw string) string {
	for _, verb := range []string{`Post "`, `Get "`, `Put "`, `Patch "`, `Delete "`} {
		start := strings.Index(raw, verb)
		if start < 0 {
			continue
		}
		rest := raw[start+len(verb):]
		end := strings.IndexByte(rest, '"')
		if end <= 0 {
			continue
		}
		if parsed, errParse := url.Parse(rest[:end]); errParse == nil && parsed.Host != "" {
			return parsed.Host
		}
	}
	return ""
}

// retryAfterSeconds 把上游明示的等待时长取整成秒（0 表示未给出）。
func retryAfterSeconds(err error) int {
	wait := RetryAfterOf(err)
	if wait <= 0 {
		return 0
	}
	seconds := int((wait + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}

// upstreamCode 生成错误码：upstream_<kind>。
func upstreamCode(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "upstream_error"
	}
	return "upstream_" + kind
}

// envelope 是 CPA 插件 ABI 的 JSON 信封，与宿主 cpasdk/pluginabi.Envelope 对齐。
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

// envelopeError 携带可选 HTTP 状态码；宿主据此决定冷却或轮转当前凭证。
type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
	// RetryAfterSeconds 是上游明示的等待秒数。新宿主会把它翻成 Retry-After
	// 响应头交给客户端；旧宿主忽略该字段，不影响兼容。
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}

// PluginError 是插件内部的结构化错误：可携带 HTTP 状态码与可重试标记。
//
// HTTPStatus 是插件与宿主之间最重要的契约之一：宿主按它决定凭证处置
// （402 长冷却 / 429 短冷却 / 401 禁用 / 403 软冷却），见 conductor_cooldown.go。
type PluginError struct {
	Code       string
	Message    string
	Retryable  bool
	HTTPStatus int
	// RetryAfterSeconds 来自上游明示的等待信息（0 表示未给出）。
	RetryAfterSeconds int
}

// Error 实现 error 接口。
func (e *PluginError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// NewPluginError 构造一个带 HTTP 状态码的插件错误。
func NewPluginError(code, message string, status int) *PluginError {
	return &PluginError{Code: code, Message: message, HTTPStatus: status}
}

// RetryablePluginError 构造一个可重试的插件错误：宿主会换一个凭证重试同一请求。
func RetryablePluginError(code, message string, status int) *PluginError {
	return &PluginError{Code: code, Message: message, Retryable: true, HTTPStatus: status}
}

// OKEnvelope 把任意结果序列化为成功信封。
func OKEnvelope(value any) ([]byte, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal result: %w", errMarshal)
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

// ErrorEnvelope 构造失败信封（无 HTTP 状态码，宿主按 500 处理）。
func ErrorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// ErrorEnvelopeFromError 把业务错误转成信封并保留 HTTP 状态码。
//
// 对 *PluginError 用类型断言而非 errors.As：插件内部没有错误包装链，
// 断言既够用又能避免把包装过的错误误判成结构化错误。
func ErrorEnvelopeFromError(err error) []byte {
	if err == nil {
		return ErrorEnvelope("plugin_error", "unknown error")
	}
	if pluginErr, ok := err.(*PluginError); ok {
		raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{
			Code:              pluginErr.Code,
			Message:           pluginErr.Message,
			Retryable:         pluginErr.Retryable,
			HTTPStatus:        pluginErr.HTTPStatus,
			RetryAfterSeconds: pluginErr.RetryAfterSeconds,
		}})
		return raw
	}
	return ErrorEnvelope("plugin_error", err.Error())
}

// ---------------------------------------------------------------------------
// 上游错误分类
//
// 各供应商只负责把上游响应翻译成这里的 FailureKind/Failure；宿主状态码、可重试
// 语义与错误码字符串统一由本文件给出。这样供应商之间不会互相 import，
// 也不会各自复制一份 HTTPStatus/Retryable 映射。

// FailureKind 是上游错误的可处置分类。
type FailureKind string

const (
	FailureKindNone            FailureKind = "none"
	FailureKindHardCredit      FailureKind = "hard_credit"
	FailureKindSoftRate        FailureKind = "soft_rate"
	FailureKindSessionDead     FailureKind = "session_dead"
	FailureKindNotFound        FailureKind = "not_found"
	FailureKindServer          FailureKind = "server"
	FailureKindContentBlocked  FailureKind = "content_blocked"
	FailureKindChannelRejected FailureKind = "channel_rejected"
	FailureKindBadParams       FailureKind = "bad_params"
	FailureKindAccountFault    FailureKind = "account_fault"
	FailureKindModelBlocked    FailureKind = "model_blocked"
	FailureKindWAFBlock        FailureKind = "waf_block"
	FailureKindPromptTooLong   FailureKind = "prompt_too_long"
	FailureKindImageInvalid    FailureKind = "image_invalid"
	FailureKindClient          FailureKind = "client"

	// 兼容通用供应商的短分类。
	FailureKindCredential FailureKind = "credential"
	FailureKindRateLimit  FailureKind = "rate_limit"
	FailureKindTransient  FailureKind = "transient"
	FailureKindQuota      FailureKind = "quota"
	FailureKindBusy       FailureKind = "busy"
)

// String 返回稳定的类别名（用于日志与错误码）。
func (k FailureKind) String() string {
	if k == "" {
		return "unknown"
	}
	return string(k)
}

// Failure 是带分类的上游错误。
//
// Msg 保持上游原文；Code 是上游业务码的可读形式（可为空）。RetryAfter /
// ResetAt 是上游明示的等待信息，零值表示未给出。
type Failure struct {
	Kind       FailureKind
	Status     int
	Code       string
	Msg        string
	RetryAfter time.Duration
	ResetAt    time.Time

	// RetryableOverride 允许供应商覆盖通用的可重试判定（nil 表示按 FailureKind 推导）。
	RetryableOverride *bool
}

// Failure 实现 error 接口。
func (e *Failure) Error() string {
	if e == nil {
		return ""
	}
	if e.Code != "" {
		return fmt.Sprintf("upstream %s [%d/%s]: %s", e.Kind, e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("upstream %s [%d]: %s", e.Kind, e.Status, e.Msg)
}

// HTTPStatus 返回映射给宿主的 HTTP 状态码。
//
// 上游状态码通常已经是宿主认得的（401/402/429/5xx），优先透传；只有
// Status==0（网络层失败或 200 包体里的业务错误）才按 FailureKind 推导。
func (e *Failure) HTTPStatus() int {
	if e == nil {
		return http.StatusBadGateway
	}
	// 账号级故障常被上游包在 400（甚至 200 的 SSE 错误帧）里返回。透传原始
	// 状态码会让宿主错过 403 软冷却分支，被封的账号于是每轮都被重新选中。
	// 只对 account_fault 强制 403：session_dead 的原始状态码另有语义
	// （zcode 的 403 要保留，宿主据此走软冷却而不是刷新凭证）。
	if e.Kind == FailureKindAccountFault {
		return http.StatusForbidden
	}
	// 只有真正的错误状态才算数：上游把业务错误塞进 200/206 包体时，透传 2xx
	// 会让宿主把「账号被封」当成成功状态码，冷却分支根本命中不了。
	if e.Status >= http.StatusBadRequest {
		return e.Status
	}
	switch e.Kind {
	case FailureKindHardCredit, FailureKindQuota:
		return http.StatusPaymentRequired
	case FailureKindSoftRate, FailureKindRateLimit:
		return http.StatusTooManyRequests
	case FailureKindSessionDead, FailureKindCredential:
		return http.StatusUnauthorized
	case FailureKindNotFound, FailureKindModelBlocked:
		return http.StatusNotFound
	case FailureKindServer, FailureKindTransient:
		return http.StatusBadGateway
	case FailureKindWAFBlock:
		return http.StatusForbidden
	case FailureKindBusy:
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadRequest
	}
}

// Retryable 报告宿主是否应该换一个凭证重试同一请求。
func (e *Failure) Retryable() bool {
	if e == nil {
		return false
	}
	if e.RetryableOverride != nil {
		return *e.RetryableOverride
	}
	switch e.Kind {
	case FailureKindContentBlocked, FailureKindChannelRejected, FailureKindPromptTooLong,
		FailureKindImageInvalid, FailureKindBadParams, FailureKindClient, FailureKindCredential:
		return false
	default:
		return true
	}
}

// UpstreamKind 返回错误分类标识。
func (e *Failure) UpstreamKind() string { return e.Kind.String() }

// UpstreamMessage 返回上游原始正文（实现 core.UpstreamDetail）。
func (e *Failure) UpstreamMessage() string {
	if e == nil {
		return ""
	}
	return e.Msg
}

// Bool 返回 bool 指针（构造 RetryableOverride 用）。
func Bool(value bool) *bool { return &value }

// ClassifyStatus 是通用 HTTP 状态分类：调用方可在此基础上追加业务码规则。
func ClassifyStatus(status int) FailureKind {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return FailureKindCredential
	case status == http.StatusPaymentRequired:
		return FailureKindHardCredit
	case status == http.StatusTooManyRequests:
		return FailureKindRateLimit
	case status >= 500:
		return FailureKindTransient
	case status >= 400:
		return FailureKindClient
	default:
		return FailureKindTransient
	}
}

// Truncate 截断过长的上游正文，避免错误信息淹没日志。
//
// 按字符边界截断而不是按字节：上游正文常是中文，按字节切会把最后一个字符
// 劈成半个 UTF-8 序列，日志与面板里显示成 U+FFFD。limit 仍按字节口径理解
// （调用方历来如此），只是切点会回退到字符边界。
func Truncate(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if limit <= 0 || len(trimmed) <= limit {
		return trimmed
	}
	return TrimToBytes(trimmed, limit) + "..."
}

// TrimToBytes 把字符串截到不超过 limit 字节，切点落在 UTF-8 字符边界上。
//
// 用于「按字节设上限、但不能切坏字符」的场合。limit <= 0 时返回空串。
func TrimToBytes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	cut := limit
	// 回退到最近的字符起始字节：UTF-8 续字节形如 10xxxxxx。
	for cut > 0 && value[cut]&0xC0 == 0x80 {
		cut--
	}
	return value[:cut]
}
