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
func ForwardRawSSE(body io.Reader, emit func([]byte) error) error {
	if body == nil {
		return nil
	}
	scanner := NewSSEScanner(body)

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
		if errEmit := emit([]byte(payload)); errEmit != nil {
			return errEmit
		}
	}
	return scanner.Err()
}
