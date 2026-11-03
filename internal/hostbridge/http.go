// Package httpx 把插件内的出站 HTTP 请求接到 CPA 宿主的 HTTP 桥上。
//
// 为什么必须走宿主桥而不是自己发请求：
//   - 宿主的全局代理、账号级代理、出站请求日志、API 统计都挂在这条链路上；
//   - 插件直接 net/http 出站会绕过以上全部，表现为"代理配了没用""请求日志里看不到"。
//
// 本包提供两个 *http.Client：
//   - Client：经 host.http.do，一次性拿到完整响应（模型清单、额度、签到等短请求）；
//   - StreamClient：经 host.http.do_stream，响应体按需从 host.http.stream_read 拉取（SSE 对话）。
//
// 对 internal 包的依赖方向：本包不知道宿存在，宿主调用能力由 package main 在
// init 里通过 Configure 注入（避免 internal 反向 import main）。
package hostbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/internal/core"
)

// 空分片保护：宿主短暂没有数据时退避；宿主持续异常时及时终止，避免永久空转。
const (
	emptyReadBackoff         = time.Millisecond
	emptyReadBackoffMax      = 16 * time.Millisecond
	maxConsecutiveEmptyReads = 32
)

// Client 返回经宿主 host.http.do 出站的客户端。
func Client(ctx context.Context, timeout time.Duration) *http.Client {
	return &http.Client{Transport: &transport{ctx: ctx}, Timeout: timeout}
}

// StreamClient 返回经宿主 host.http.do_stream 出站的客户端（用于 SSE）。
//
// 超时设为 0：流式响应的总时长不可预期，首字节与空闲超时分别由上游
// ResponseHeaderTimeout 与 idle 监控负责（见 WorkBuddy 包的 idle 处理）。
func StreamClient(ctx context.Context, timeout time.Duration) *http.Client {
	return &http.Client{Transport: &transport{ctx: ctx, stream: true}, Timeout: timeout}
}

type transport struct {
	ctx    context.Context
	stream bool
}

// RoundTrip 实现 http.RoundTripper：把请求转成宿主 HTTP 桥调用。
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if t.ctx != nil {
		ctx = t.ctx
	}
	body, errBody := readRequestBody(req)
	if errBody != nil {
		return nil, errBody
	}

	payload := rpcHostHTTPRequest{
		Method:  req.Method,
		URL:     req.URL.String(),
		Headers: req.Header,
		Body:    body,
	}
	method := pluginabi.MethodHostHTTPDo
	if t.stream {
		method = pluginabi.MethodHostHTTPDoStream
	}
	raw, errCall := CallCtx(ctx, method, payload)
	if errCall != nil {
		return nil, errCall
	}

	if !t.stream {
		var rpc rpcHostHTTPResponse
		if errUnmarshal := json.Unmarshal(raw, &rpc); errUnmarshal != nil {
			return nil, fmt.Errorf("decode host http response: %w", errUnmarshal)
		}
		if !rpc.statusSeen {
			// 不能静默当 0：那会让上游 200 也被判成失败，而错误体里又带着真实响应，
			// 看起来像"凭证被拒"，极难排查。宁可直接报协议不匹配。
			return nil, fmt.Errorf("host http response has no status field (host ABI mismatch): %s", truncateForError(raw))
		}
		return buildResponse(req, rpc.StatusCode, rpc.Headers, io.NopCloser(bytes.NewReader(rpc.Body))), nil
	}

	var rpc rpcHostHTTPStreamResponse
	if errUnmarshal := json.Unmarshal(raw, &rpc); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host http stream response: %w", errUnmarshal)
	}
	if rpc.StatusCode == 0 {
		return nil, fmt.Errorf("host http stream response has no status field (host ABI mismatch): %s", truncateForError(raw))
	}
	streamBody := &streamBody{
		ctx:      ctx,
		streamID: rpc.StreamID,
	}
	return buildResponse(req, rpc.StatusCode, rpc.Headers, streamBody), nil
}

func readRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	body, errRead := io.ReadAll(req.Body)
	if errClose := req.Body.Close(); errClose != nil && errRead == nil {
		return nil, fmt.Errorf("close request body: %w", errClose)
	}
	if errRead != nil {
		return nil, fmt.Errorf("read request body: %w", errRead)
	}
	return body, nil
}

func buildResponse(req *http.Request, status int, header http.Header, body io.ReadCloser) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          body,
		ContentLength: -1,
		Request:       req,
	}
}

// rpcHostHTTPRequest 是 host.http.do / do_stream 的请求体。
//
// 只填扁平字段（method/url/headers/body）：宿主对扁平与嵌套两种形态都接受，
// 扁平形态更简单且不容易漏字段。
type rpcHostHTTPRequest struct {
	Method  string      `json:"method,omitempty"`
	URL     string      `json:"url,omitempty"`
	Headers http.Header `json:"headers,omitempty"`
	Body    []byte      `json:"body,omitempty"`
}

// rpcHostHTTPResponse 是 host.http.do 的响应体。
//
// 关键：宿主的 host.http.do 直接把 pluginapi.HTTPResponse 丢进信封，而该结构体
// **没有 json tag**，因此线上键名是 Go 字段名（StatusCode / Headers / Body）。
// 但宿主少数地方会走带 tag 的中间结构体（status_code / headers / body）。
// 只认一种的话状态码会静默变成 0，后果是所有非流式上游请求全部被判成失败。
// 因此这里手写 UnmarshalJSON 同时接受两种形态。
type rpcHostHTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	// statusSeen 标记是否真的解到了状态码。
	// 用「见过」而不是「缺失」是为了让零值语义正确：结构体零值即「没见过」。
	statusSeen bool
}

// UnmarshalJSON 兼容宿主两种序列化形态（Go 字段名 / snake_case）。
func (r *rpcHostHTTPResponse) UnmarshalJSON(data []byte) error {
	if r == nil {
		return errors.New("nil host http response")
	}
	var fields map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(data, &fields); errUnmarshal != nil {
		return errUnmarshal
	}
	for key, value := range fields {
		switch strings.ToLower(strings.ReplaceAll(key, "_", "")) {
		case "statuscode":
			r.statusSeen = true
			if errDecode := json.Unmarshal(value, &r.StatusCode); errDecode != nil {
				return fmt.Errorf("decode host http status: %w", errDecode)
			}
		case "headers":
			if errDecode := json.Unmarshal(value, &r.Headers); errDecode != nil {
				return fmt.Errorf("decode host http headers: %w", errDecode)
			}
		case "body":
			if errDecode := json.Unmarshal(value, &r.Body); errDecode != nil {
				return fmt.Errorf("decode host http body: %w", errDecode)
			}
		}
	}
	return nil
}

// rpcHostHTTPStreamResponse 是 host.http.do_stream 的响应体。
// 这个结构体在宿主侧**有** json tag，键名是 snake_case。
type rpcHostHTTPStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

type rpcHostHTTPStreamReadRequest struct {
	StreamID string `json:"stream_id"`
}

type rpcHostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type rpcHostHTTPStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
}

// streamBody 实现 io.ReadCloser，按需从 host.http.stream_read 拉取上游分片。
//
// 与宿主的约定：宿主返回 done=true 表示流结束；error 非空表示流错误。
// 本类型负责把这两者翻译成 io.EOF 与 error。
type streamBody struct {
	ctx      context.Context
	streamID string

	mu       sync.Mutex
	closed   bool
	closeErr error
	pending  []byte
}

// Read 从上游流读取数据。每次调用向宿主索要一个分片。
func (b *streamBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.mu.Lock()
	if len(b.pending) > 0 {
		n := copy(p, b.pending)
		b.pending = b.pending[n:]
		b.mu.Unlock()
		return n, nil
	}
	if b.closed {
		errClosed := b.closeErr
		b.mu.Unlock()
		if errClosed != nil {
			return 0, errClosed
		}
		return 0, io.EOF
	}
	b.mu.Unlock()

	chunk, errRead := b.readChunk()
	if errRead != nil {
		// 读失败即终结：标记关闭并关闭宿主侧流，避免泄漏。
		_ = b.Close()
		return 0, errRead
	}
	if len(chunk) == 0 {
		return 0, nil
	}
	n := copy(p, chunk)
	if n < len(chunk) {
		b.mu.Lock()
		b.pending = append(b.pending, chunk[n:]...)
		b.mu.Unlock()
	}
	return n, nil
}

// errTooManyEmptyReads 表示宿主持续返回空分片，流已被主动终止。
var errTooManyEmptyReads = errors.New("host http stream returned too many empty chunks")

// readChunk 向宿主索要一个分片。
//
// 空分片不是结束信号：宿主桥接在缓冲区暂时为空时会返回 payload=nil。处理方式
// 是退避重试，并给单次调用里的连续空分片设上限——桥接异常时不能永久空转。
// 每次 Read 都是一次新的 readChunk，计数器与退避自然从头开始。
func (b *streamBody) readChunk() ([]byte, error) {
	if strings.TrimSpace(b.streamID) == "" {
		return nil, errors.New("host http stream id is missing")
	}
	emptyReads := 0
	backoff := emptyReadBackoff
	for {
		select {
		case <-b.ctx.Done():
			return nil, b.ctx.Err()
		default:
		}
		raw, errCall := CallCtx(b.ctx, pluginabi.MethodHostHTTPStreamRead, rpcHostHTTPStreamReadRequest{StreamID: b.streamID})
		if errCall != nil {
			return nil, fmt.Errorf("host http stream read: %w", errCall)
		}
		var chunk rpcHostHTTPStreamReadResponse
		if errUnmarshal := json.Unmarshal(raw, &chunk); errUnmarshal != nil {
			return nil, fmt.Errorf("decode host http stream chunk: %w", errUnmarshal)
		}
		if chunk.Error != "" {
			return nil, errors.New(chunk.Error)
		}
		if chunk.Done {
			return nil, io.EOF
		}
		if len(chunk.Payload) > 0 {
			return chunk.Payload, nil
		}
		emptyReads++
		if emptyReads >= maxConsecutiveEmptyReads {
			return nil, errTooManyEmptyReads
		}
		// 空分片：指数退避让出 CPU，直到有数据、结束或 context 取消。
		select {
		case <-b.ctx.Done():
			return nil, b.ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < emptyReadBackoffMax {
			backoff *= 2
			if backoff > emptyReadBackoffMax {
				backoff = emptyReadBackoffMax
			}
		}
	}
}

// Close 关闭宿主侧的上游流。幂等：只会真正调用一次 host.http.stream_close。
func (b *streamBody) Close() error {
	b.mu.Lock()
	if b.closed {
		errClosed := b.closeErr
		b.mu.Unlock()
		return errClosed
	}
	b.closed = true
	b.mu.Unlock()

	if strings.TrimSpace(b.streamID) == "" {
		return nil
	}
	_, errCall := CallCtx(b.ctx, pluginabi.MethodHostHTTPStreamClose, rpcHostHTTPStreamCloseRequest{StreamID: b.streamID})
	if errCall != nil {
		errWrapped := fmt.Errorf("host http stream close: %w", errCall)
		b.mu.Lock()
		b.closeErr = errWrapped
		b.mu.Unlock()
		return errWrapped
	}
	return nil
}

// truncateForError 截断用于错误信息的响应体，避免把整页 HTML 塞进日志。
//
// 按 UTF-8 字符边界截断：上游错误页多为中文，按字节切会把最后一个字符劈成
// 半个非法序列，日志与错误帧里就会带一串替换字符。
func truncateForError(raw []byte) string {
	const limit = 256
	if len(raw) <= limit {
		return string(raw)
	}
	return core.TrimToBytes(string(raw), limit) + "..."
}
