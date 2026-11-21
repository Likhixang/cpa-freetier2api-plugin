package hostbridge

// 本文件提供「上游 SSE 与宿主契约同形」时的原样转发。
//
// 为什么放在 httpx：它只依赖 io，属于出站/入站的通用工具，不属于任何供应商。
// 供应商相关的 SSE 改写（WorkBuddy 的白名单重建、Qoder 的协议转换）仍留在
// 各自的包里。

import (
	"bufio"
	"io"
	"strings"

	"freetier2api-plugin/internal/core"
)

// SSEScannerBuffer 是单帧的上限（默认 64KB 会截断工具参数这类长帧）。
const SSEScannerBuffer = 8 * 1024 * 1024

// sseScannerInitialBuffer 是读流 Scanner 的初始缓冲，长行时按需增长到 SSEScannerBuffer。
const sseScannerInitialBuffer = 64 * 1024

// NewSSEScanner 返回按行读取上游流式响应的 Scanner：初始缓冲 64 KiB，单行上限
// SSEScannerBuffer。各供应商读流统一用它创建，避免各自的上限不一致（超限会
// 报 bufio.ErrTooLong 并中断整条流）。
func NewSSEScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, sseScannerInitialBuffer), SSEScannerBuffer)
	return scanner
}

// ForwardRawSSE 逐帧转发上游 SSE 载荷。
//
// 行为约定（与宿主契约对齐）：
//   - `data: ` 前缀由宿主补，这里只交载荷；
//   - `[DONE]` 是流结束信号，不转发（宿主自己会补）；
//   - 非 data 行（注释、event:）原样转发，保持上游帧结构；
//   - 空载荷跳过。
//
// 全程零有效帧不算错误：上游有时返回一条没有任何 delta 的流，
// 把它当错误会让客户端收到无意义的错误帧。
//
// 出过有效帧却没有结束帧就 EOF，按 core.ErrStreamTruncated 返回：宿主会把
// 这类失败当成 stream_truncated（不冷却凭证、不重放请求），而不是当成上游
// 挂了去冷却一个其实好用的账号。
func ForwardRawSSE(body io.Reader, emit func([]byte) error) error {
	if body == nil {
		return nil
	}
	scanner := NewSSEScanner(body)
	emitted := 0
	sawTerminal := false

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "data:") {
			// 注释与 event: 行原样下发，保持上游的帧结构。
			if errEmit := emit([]byte(line)); errEmit != nil {
				return errEmit
			}
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "[DONE]" {
			return nil
		}
		if payload == "" {
			continue
		}
		if isTerminalSSEPayload(payload) {
			sawTerminal = true
		}
		if errEmit := emit([]byte(payload)); errEmit != nil {
			return errEmit
		}
		emitted++
	}
	if errScan := scanner.Err(); errScan != nil {
		return errScan
	}
	if emitted > 0 && !sawTerminal {
		return core.ErrStreamTruncated
	}
	return nil
}

// isTerminalSSEPayload 判断这一帧是否携带了「本轮生成已结束」的信号。
//
// 判据刻意宽松：finish_reason / finishReason / stop_reason / event: message_stop
// 任一出现都算结束。误判的代价只是不冷却凭证（比冷却一个好账号轻），
// 漏判的代价是把正常收尾的流当成截断 —— 所以宁可放宽。
func isTerminalSSEPayload(payload string) bool {
	for _, marker := range []string{`"finish_reason"`, `"finishReason"`, `"stop_reason"`, `message_stop`} {
		if strings.Contains(payload, marker) {
			return true
		}
	}
	return false
}
