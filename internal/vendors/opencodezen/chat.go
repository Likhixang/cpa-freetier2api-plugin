package opencodezen

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// 本文件实现 OpenCode ZEN 的 chat-completions 转发。
//
// 特殊要求（参考 opencode2api 与 OpenCode 官方实现）：
//   1. 会话 ID 必须是 canonical session 格式：`ses_` + 12位十六进制 + 14位 Base62。
//      时间戳部分锚定在近期合理窗口内（30 天内），避免旧实现哈希时间落到 1970 年而触发
//      上游 429 FreeUsageLimitError；进程启动时生成随机盐值（bootNonce），避免重启后沿用
//      已在风控状态的会话桶。
//   2. 项目 ID 不能使用固定的 "global"，上游按 (project, egress) 限额记账；缺省时生成
//      官方 CLI 规格的 40 位十六进制项目标识，避免共享已被耗尽的配额桶。
//   3. 客户端工具名称需混淆为 clint_<sha256[:12]>，防止因携带 Claude Code 等外部工具签名
//      触发上游的客户端指纹风控而返回 429 FreeUsageLimitError。返回时自动恢复原工具名。
//   4. 免费层模型（单价为 0）走 inference 车道、匿名 public 凭证、强制 stream=true，
//      并在非流式调用下通过 CollapseSSEToJSON 合并为标准 JSON 响应。

const (
	// chatHTTPTimeout 是非流式对话的上限。
	chatHTTPTimeout = 10 * time.Minute

	// clientToolPrefix 是免费层工具混淆前缀。
	clientToolPrefix = "clint_"
	// clientToolDigestLength 是混淆摘要的十六进制字符长度。
	clientToolDigestLength = 12

	// sessionTimestampWindow 限制派生会话时间戳距今日 UTC 0 点的最大回退跨度（30 天）。
	sessionTimestampWindow = 30 * 24 * time.Hour

	// base62Alphabet 是 Base62 字符集。
	base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// ChatRequest 是一次对话转发的输入。
type ChatRequest struct {
	// APIKey 是付费模型的出站凭证；免费层改用匿名 public 凭证。
	APIKey string
	// Body 是宿主翻译好的 chat-completions 请求体。
	Body []byte
	// Headers 是客户端原始请求头（用于透传会话与项目信息）。
	Headers map[string][]string
	// Stream 报告是否流式。
	Stream bool
	// Catalog 是 models.dev 目录，用于按单价判定免费层。
	// 为空时退回按模型名判定。
	Catalog modelsDevCatalog
}

// toolRenames 维护客户端工具名与混淆线名之间的双向映射。
type toolRenames struct {
	toWire   map[string]string
	toClient map[string]string
}

func (r *toolRenames) add(client, wire string) {
	if r.toWire == nil {
		r.toWire = make(map[string]string)
		r.toClient = make(map[string]string)
	}
	r.toWire[client] = wire
	r.toClient[wire] = client
}

// Chat 执行一次非流式对话，返回上游的完整响应体。
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	modelName := extractModelName(req.Body)
	isFree := isFreeTierModel(modelName, req.Catalog)

	if isFree {
		// 免费层只接流式：强制 stream 打开，再把 SSE 聚合回完整 JSON 文档。
		req.Stream = true
		resp, renames, errStream := ChatStream(ctx, baseURL, req)
		if errStream != nil {
			return nil, errStream
		}
		defer func() { _ = resp.Body.Close() }()

		return CollapseSSEToJSON(resp.Body, modelName, renames)
	}

	resp, _, errDo := doChat(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read chat response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	return body, nil
}

// ChatStream 执行一次流式对话，返回上游响应体及工具名还原映射（调用方负责关闭）。
func ChatStream(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, *toolRenames, error) {
	httpReq, renames, errReq := newChatRequest(ctx, baseURL, req, req.Catalog)
	if errReq != nil {
		return nil, nil, errReq
	}

	resp, errDo := hostbridge.StreamClient(ctx, 0).Do(httpReq)
	if errDo != nil {
		return nil, nil, &core.Failure{Msg: "chat stream: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, nil, Classify(resp.StatusCode, string(body))
	}
	return resp, renames, nil
}

// doChat 发起一次非流式对话请求。
func doChat(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, *toolRenames, error) {
	httpReq, renames, errReq := newChatRequest(ctx, baseURL, req, req.Catalog)
	if errReq != nil {
		return nil, nil, errReq
	}

	resp, errDo := hostbridge.Client(ctx, chatHTTPTimeout).Do(httpReq)
	if errDo != nil {
		return nil, nil, &core.Failure{Msg: "chat: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	return resp, renames, nil
}

// newChatRequest 组装一次 chat-completions 请求（流式与非流式共用）。
func newChatRequest(ctx context.Context, baseURL string, req ChatRequest, catalog modelsDevCatalog) (*http.Request, *toolRenames, error) {
	body := req.Body
	credential := req.APIKey
	var renames *toolRenames

	if isFreeTierModel(extractModelName(body), catalog) {
		body, renames = prepareFreeTierBody(body)
		credential = freeTierCredential
	} else {
		body = normalizeMaxTokens(body)
	}

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, chatEndpoint(baseURL), bytesReader(body))
	if errReq != nil {
		return nil, nil, fmt.Errorf("build chat request: %w", errReq)
	}

	sessionID := canonicalSessionID(extractSessionID(req.Headers, body))
	projectID := extractProjectID(req.Headers, body)
	ApplyChatHeaders(httpReq, credential, sessionID, projectID)
	return httpReq, renames, nil
}

// normalizeMaxTokens 把 max_tokens 归一到 max_completion_tokens。
func normalizeMaxTokens(body []byte) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	value, hasMax := payload["max_tokens"]
	if !hasMax {
		return body
	}
	if _, hasCompletion := payload["max_completion_tokens"]; !hasCompletion {
		payload["max_completion_tokens"] = value
	}
	delete(payload, "max_tokens")
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

// ensureCompletionLimit 规范化输出上限并在缺省时设置默认上限 32000。
func ensureCompletionLimit(payload map[string]any) {
	if val, ok := payload["max_tokens"]; ok {
		if _, hasComp := payload["max_completion_tokens"]; !hasComp {
			payload["max_completion_tokens"] = val
		}
		delete(payload, "max_tokens")
	}
	if _, ok := payload["max_completion_tokens"]; !ok {
		payload["max_completion_tokens"] = 32000
	}
}

// chatEndpoint 返回 chat-completions 的完整地址。
func chatEndpoint(override string) string {
	return BaseURL(override) + ChatPath
}

// extractModelName 从请求体中提取 model 字符串。
func extractModelName(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &payload)
	return payload.Model
}

// isFreeTierModel 判断是否为免费层模型。
func isFreeTierModel(model string, catalog modelsDevCatalog) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	if free, known := catalogIsFree(catalog, model); known {
		return free
	}
	return strings.Contains(strings.ToLower(model), "free")
}

// catalogIsFree 用目录里该模型的单价回答「是否免费」。
func catalogIsFree(catalog modelsDevCatalog, model string) (free bool, known bool) {
	entry, okEntry := catalog[model]
	if !okEntry || entry.Cost == nil {
		return false, false
	}
	return entry.Cost.Input == 0 && entry.Cost.Output == 0, true
}

// clientToolWireName 派生不透明工具线名：clint_ + 12位 SHA-256 十六进制。
// 解决 Claude Code 等客户端携带特征工具名时被上游风控判定为 429 的问题。
func clientToolWireName(name string, used map[string]bool) string {
	sum := sha256.Sum256([]byte(name))
	base := clientToolPrefix + hex.EncodeToString(sum[:])[:clientToolDigestLength]
	candidate := base
	for i := 2; used[candidate]; i++ {
		candidate = fmt.Sprintf("%s_%d", base, i)
	}
	used[candidate] = true
	return candidate
}

// rewriteToolReferences 同步改写 tool_choice 与 messages 历史记录中的工具名。
func rewriteToolReferences(payload map[string]any, renames *toolRenames) {
	if renames == nil || len(renames.toWire) == 0 {
		return
	}
	if choice, ok := payload["tool_choice"].(map[string]any); ok {
		if fn, ok := choice["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				if wire, ok := renames.toWire[name]; ok {
					fn["name"] = wire
				}
			}
		} else if name, ok := choice["name"].(string); ok {
			if wire, ok := renames.toWire[name]; ok {
				choice["name"] = wire
			}
		}
	}
	if msgs, ok := payload["messages"].([]any); ok {
		for _, rawMsg := range msgs {
			msg, ok := rawMsg.(map[string]any)
			if !ok {
				continue
			}
			if calls, ok := msg["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					call, ok := rawCall.(map[string]any)
					if !ok {
						continue
					}
					if fn, ok := call["function"].(map[string]any); ok {
						if name, ok := fn["name"].(string); ok {
							if wire, ok := renames.toWire[name]; ok {
								fn["name"] = wire
							}
						}
					}
				}
			}
			if fnCall, ok := msg["function_call"].(map[string]any); ok {
				if name, ok := fnCall["name"].(string); ok {
					if wire, ok := renames.toWire[name]; ok {
						fnCall["name"] = wire
					}
				}
			}
		}
	}
}

// prepareFreeTierBody 规范化免费层请求体：强制流式、用量统计、上限归一与工具混淆。
func prepareFreeTierBody(body []byte) ([]byte, *toolRenames) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}

	payload["stream"] = true
	if opt, ok := payload["stream_options"].(map[string]any); ok {
		opt["include_usage"] = true
	} else {
		payload["stream_options"] = map[string]any{"include_usage": true}
	}

	ensureCompletionLimit(payload)

	var renames *toolRenames
	if rawTools, ok := payload["tools"].([]any); ok && len(rawTools) > 0 {
		renames = &toolRenames{toWire: make(map[string]string), toClient: make(map[string]string)}
		used := make(map[string]bool)
		for _, rawTool := range rawTools {
			toolMap, okTool := rawTool.(map[string]any)
			if !okTool {
				continue
			}
			if fn, okFn := toolMap["function"].(map[string]any); okFn {
				if name, okName := fn["name"].(string); okName && name != "" && !strings.HasPrefix(name, clientToolPrefix) {
					wireName := clientToolWireName(name, used)
					renames.add(name, wireName)
					fn["name"] = wireName
				}
			} else if name, okName := toolMap["name"].(string); okName && name != "" && !strings.HasPrefix(name, clientToolPrefix) {
				wireName := clientToolWireName(name, used)
				renames.add(name, wireName)
				toolMap["name"] = wireName
			}
		}
		rewriteToolReferences(payload, renames)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return body, nil
	}
	return encoded, renames
}

type chatCompletionResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

type chatMessage struct {
	Role             string `json:"role"`
	Content          any    `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
	ToolCalls        []any  `json:"tool_calls,omitempty"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type sseDeltaChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []any  `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

// CollapseSSEToJSON 将上游 SSE 流消费并组装成一个完整的 OpenAI 格式 chat.completion JSON 响应。
func CollapseSSEToJSON(r io.Reader, defaultModel string, renames ...*toolRenames) ([]byte, error) {
	// 单帧上限：默认 64KB 会被长工具参数/长思考块撑爆，撑爆后 Scan 返回 false
	// 且 Err() 是 ErrTooLong，整段回答会被静默截断。
	scanner := hostbridge.NewSSEScanner(r)
	resp := chatCompletionResponse{
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   defaultModel,
		Choices: []chatChoice{
			{
				Index: 0,
				Message: chatMessage{
					Role: "assistant",
				},
				FinishReason: "stop",
			},
		},
	}

	var contentBuilder strings.Builder
	var reasoningBuilder strings.Builder
	var toolCalls []any

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		// 上游会用 200 + {"error":{...}} 的帧表达失败（FreeTierError、限流、
		// 上游过载）。不认这一帧，失败就被折叠成一条 content 为空的 200 响应，
		// 客户端只看到「模型什么都没说」。
		if failure := frameFailure(data); failure != nil {
			return nil, failure
		}
		var chunk sseDeltaChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.ID != "" {
			resp.ID = chunk.ID
		}
		if chunk.Model != "" {
			resp.Model = chunk.Model
		}
		if chunk.Created > 0 {
			resp.Created = chunk.Created
		}
		if chunk.Usage != nil {
			resp.Usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				contentBuilder.WriteString(choice.Delta.Content)
			}
			if choice.Delta.Reasoning != "" {
				reasoningBuilder.WriteString(choice.Delta.Reasoning)
			} else if choice.Delta.ReasoningContent != "" {
				reasoningBuilder.WriteString(choice.Delta.ReasoningContent)
			}
			if len(choice.Delta.ToolCalls) > 0 {
				toolCalls = mergeToolCalls(toolCalls, choice.Delta.ToolCalls)
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				resp.Choices[0].FinishReason = *choice.FinishReason
			}
		}
	}

	// 扫描中断（帧超长 / 读错误）必须上抛：静默返回半段回答比报错更糟。
	if errScan := scanner.Err(); errScan != nil {
		return nil, &core.Failure{Kind: core.FailureKindTransient, Msg: "read sse stream: " + errScan.Error()}
	}

	contentStr := contentBuilder.String()
	resp.Choices[0].Message.Content = contentStr
	resp.Choices[0].Message.ReasoningContent = reasoningBuilder.String()

	toolCalls = filterValidToolCalls(toolCalls)
	if len(toolCalls) > 0 {
		if len(renames) > 0 && renames[0] != nil && len(renames[0].toClient) > 0 {
			mapping := renames[0].toClient
			for _, call := range toolCalls {
				if callMap, ok := call.(map[string]any); ok {
					if fn, ok := callMap["function"].(map[string]any); ok {
						if name, ok := fn["name"].(string); ok {
							if original, ok := mapping[name]; ok {
								fn["name"] = original
							}
						}
					}
				}
			}
		}
		resp.Choices[0].Message.ToolCalls = toolCalls
		resp.Choices[0].FinishReason = "tool_calls"
		if contentStr == "" {
			resp.Choices[0].Message.Content = nil
		}
	}

	return json.Marshal(resp)
}

// frameFailure 识别 SSE 数据帧里的错误信封。
//
// 只在确实带 error 字段时返回失败：正常帧没有这个键，误判会把好帧当错误。
func frameFailure(data string) error {
	var frame struct {
		Error json.RawMessage `json:"error"`
	}
	if errUnmarshal := json.Unmarshal([]byte(data), &frame); errUnmarshal != nil {
		return nil
	}
	trimmed := strings.TrimSpace(string(frame.Error))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	// status 传 0：错误发生在「传输成功」的帧里，分类由正文决定
	// （FreeTierError 归 client，不罚账号）。
	return Classify(0, data)
}

func mergeToolCalls(accumulated []any, incoming []any) []any {
	for _, raw := range incoming {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		idx := core.IntFromAny(call["index"])
		var target map[string]any
		for _, item := range accumulated {
			if existing, okMap := item.(map[string]any); okMap {
				if core.IntFromAny(existing["index"]) == idx {
					target = existing
					break
				}
			}
		}
		if target == nil {
			target = cloneToolCall(call)
			target["index"] = idx
			accumulated = append(accumulated, target)
			continue
		}
		if id, okID := call["id"].(string); okID && id != "" {
			target["id"] = id
		}
		if tp, okTp := call["type"].(string); okTp && tp != "" {
			target["type"] = tp
		}
		if fnIncoming, okFn := call["function"].(map[string]any); okFn {
			fnTarget, _ := target["function"].(map[string]any)
			if fnTarget == nil {
				fnTarget = map[string]any{}
				target["function"] = fnTarget
			}
			if name, okName := fnIncoming["name"].(string); okName && name != "" {
				fnTarget["name"] = name
			}
			if args, okArgs := fnIncoming["arguments"].(string); okArgs && args != "" {
				prevArgs, _ := fnTarget["arguments"].(string)
				fnTarget["arguments"] = prevArgs + args
			}
		}
	}
	return accumulated
}

func filterValidToolCalls(calls []any) []any {
	var valid []any
	for _, item := range calls {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, okFn := call["function"].(map[string]any)
		if !okFn {
			continue
		}
		name, _ := fn["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		valid = append(valid, call)
	}
	for i := 1; i < len(valid); i++ {
		for j := i; j > 0; j-- {
			left, _ := valid[j-1].(map[string]any)
			right, _ := valid[j].(map[string]any)
			if core.IntFromAny(left["index"]) > core.IntFromAny(right["index"]) {
				valid[j-1], valid[j] = valid[j], valid[j-1]
			}
		}
	}
	return valid
}

func cloneToolCall(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		if k == "function" {
			if fn, ok := v.(map[string]any); ok {
				fnCopy := make(map[string]any, len(fn))
				for fk, fv := range fn {
					fnCopy[fk] = fv
				}
				dst[k] = fnCopy
				continue
			}
		}
		dst[k] = v
	}
	return dst
}

// bootNonce 是进程级会话盐值，每次进程启动时生成一次。
// 免费层会将配额/风险与 session id 绑定；重启后注入新盐值以重置上游 session，避免沿用已 429 的会话桶。
var bootNonce = func() []byte {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return []byte("opencode-zen-salt")
	}
	return buf
}()

// canonicalSessionPattern 匹配 OpenCode 官方的 session 形态：
// "ses_" + 12 位小写十六进制 + 14 位 Base62。
var canonicalSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// descendingTimePartPattern 匹配官方客户端 Identifier.descending() 铸造的
// 时间部分：48 位值 ~(ts_ms<<12|counter) 的十六进制。按位取反会把低 12 位
// 置成 0xFFF-counter，因此真实时间部分以 f[c-f] 收尾。
var descendingTimePartPattern = regexp.MustCompile(`^[0-9a-f]{9}f[c-f][0-9a-f]$`)

// sessionIDFor 由请求体派生本次请求的 session ID。
func sessionIDFor(body []byte) string {
	return canonicalSessionID(conversationSeed(body))
}

func extractSessionID(headers map[string][]string, body []byte) string {
	for _, key := range []string{
		"x-opencode-session",
		"x-opencode-session-id",
		"x-session-affinity",
		"x-session-id",
		"session_id",
	} {
		if vals, ok := headers[key]; ok && len(vals) > 0 && strings.TrimSpace(vals[0]) != "" {
			return strings.TrimSpace(vals[0])
		}
	}
	return conversationSeed(body)
}

func extractProjectID(headers map[string][]string, body []byte) string {
	for _, key := range []string{"x-opencode-project", "project_id"} {
		if vals, ok := headers[key]; ok && len(vals) > 0 && strings.TrimSpace(vals[0]) != "" {
			proj := strings.TrimSpace(vals[0])
			if proj != "global" {
				return proj
			}
		}
	}
	var payload struct {
		Metadata struct {
			ProjectID string `json:"project_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Metadata.ProjectID != "" {
		if payload.Metadata.ProjectID != "global" {
			return payload.Metadata.ProjectID
		}
	}
	return randomProjectID()
}

// randomProjectID 生成官方 CLI 规格的项目标识（40 位十六进制 git root commit 格式）。
// 免费层会按 (project, egress) 联合记账，固定为 "global" 会导致所有人共享已被耗尽的配额桶。
func randomProjectID() string {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "0000000000000000000000000000000000000000"
	}
	return hex.EncodeToString(buf)
}

// canonicalSessionID 把任意信号确定性地重编码成官方 session 形态。
// 已经是官方铸造（形态与时间部分编码都对）的信号原样保留，以免破坏亲和性。
func canonicalSessionID(signal string) string {
	if canonicalSessionPattern.MatchString(signal) &&
		descendingTimePartPattern.MatchString(signal[4:16]) {
		return signal
	}
	hasher := sha256.New()
	hasher.Write([]byte("ses\x00"))
	hasher.Write(bootNonce)
	hasher.Write([]byte{0})
	hasher.Write([]byte(signal))
	sum := hasher.Sum(nil)
	timePart := descendingTimePart(time.Now(), sum[:6])
	randomPart := base62Fixed(new(big.Int).SetBytes(sum[6:16]), 14)
	return "ses_" + timePart + randomPart
}

// conversationSeed 取首条 user 消息的序列化内容作为会话种子。
func conversationSeed(body []byte) string {
	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		PreviousResponseID string `json:"previous_response_id"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return randomSessionSignal()
	}
	for _, message := range payload.Messages {
		if message.Role != "user" || message.Content == nil {
			continue
		}
		encoded, errMarshal := json.Marshal(message.Content)
		if errMarshal == nil && string(encoded) != "null" {
			return string(encoded)
		}
	}
	if trimmed := strings.TrimSpace(payload.PreviousResponseID); trimmed != "" {
		return trimmed
	}
	return randomSessionSignal()
}

// randomSessionSignal 在请求体给不出任何信号时提供一次性种子。
func randomSessionSignal() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(buf)
}

// descendingTimePart 渲染一个近期合理时间戳的官方 Identifier.descending() 编码：
// 36 位时间戳顶到 48 位高位，OR 上计数器 1，再按位取反，低 12 位读作 ffe。
// 时间戳锚定在今日 UTC 0 点并按哈希在 30 天窗口内取偏移，保证合法且稳定。
func descendingTimePart(now time.Time, hash []byte) string {
	window := uint64(sessionTimestampWindow / time.Millisecond)
	offset := (uint64(hash[0])<<24 | uint64(hash[1])<<16 | uint64(hash[2])<<8 | uint64(hash[3])) % window
	base := now.UTC().Truncate(24 * time.Hour).Add(-time.Duration(offset) * time.Millisecond)
	current := uint64(base.UnixMilli())<<12 | 1
	inverted := ^current & 0xFFFFFFFFFFFF
	return hex.EncodeToString([]byte{
		byte(inverted >> 40), byte(inverted >> 32), byte(inverted >> 24),
		byte(inverted >> 16), byte(inverted >> 8), byte(inverted),
	})
}

func base62Fixed(n *big.Int, width int) string {
	base := big.NewInt(62)
	out := make([]byte, width)
	remainder := new(big.Int)
	for i := width - 1; i >= 0; i-- {
		n.DivMod(n, base, remainder)
		out[i] = base62Alphabet[remainder.Int64()]
	}
	return string(out)
}

// bytesReader 把字节切片包成 io.Reader。
func bytesReader(data []byte) io.Reader {
	return bytes.NewReader(data)
}

const (
	// DefaultBaseURL 是 chat-completions 的上游基地址。
	DefaultBaseURL = "https://opencode.ai/inference/openai"
	// ChatPath 是 chat-completions 的路径。
	ChatPath = "/v1/chat/completions"
	// ModelsBaseURL 是模型清单的基地址，比 chat 少一层 /openai。
	ModelsBaseURL = "https://opencode.ai/inference"
	// ModelsPath 是模型清单的路径。
	ModelsPath = "/v1/models"
	// freeTierCredential 是官方客户端为免费层使用的匿名凭证。
	freeTierCredential = "public"

	// clientVersion 是伪装用的 OpenCode 客户端版本，对齐 v2 稳定版。
	clientVersion = "2.1.4"
	// clientChannel 是官方稳定通道名。
	clientChannel = "latest"
	// clientName 是官方 CLI 的客户端标识（x-opencode-client 与 UA 共用）。
	clientName = "cli"
	// userAgentFormat 是 v2 的 UA 形态：opencode/<channel>/<version>/<name>。
	userAgentFormat = "opencode/%s/%s/%s"
)

// BaseURL 返回 chat-completions 的上游基地址（末尾无斜杠）。
func BaseURL(override string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(override), "/")
	if trimmed == "" {
		return DefaultBaseURL
	}
	return trimmed
}

// ModelsBaseURLFor 返回模型清单的上游基地址（末尾无斜杠）。
func ModelsBaseURLFor(override string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(override), "/")
	if trimmed == "" {
		return ModelsBaseURL
	}
	return strings.TrimSuffix(trimmed, "/openai")
}

// UserAgent 返回伪装成官方 OpenCode 客户端的 UA。
func UserAgent() string {
	return fmt.Sprintf(userAgentFormat, clientChannel, clientVersion, clientName)
}

// ApplyChatHeaders 写入 chat-completions 请求所需的全部头。
//
// 认证与客户端标识是硬门槛：ZEN 上游按 x-opencode-client 与 UA 判定请求来源。
func ApplyChatHeaders(req *http.Request, apiKey, sessionID string, projectID ...string) {
	proj := ""
	if len(projectID) > 0 {
		proj = projectID[0]
	}
	if proj == "" || proj == "global" {
		proj = randomProjectID()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", UserAgent())
	req.Header.Set("x-opencode-client", clientName)
	req.Header.Set("x-opencode-session", sessionID)
	req.Header.Set("x-opencode-session-id", sessionID)
	req.Header.Set("x-session-affinity", sessionID)
	req.Header.Set("X-Session-Id", sessionID)
	req.Header.Set("x-opencode-project", proj)
}

// ApplyModelsHeaders 写入模型清单请求的头。
func ApplyModelsHeaders(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", UserAgent())
	req.Header.Set("x-opencode-client", clientName)
}
