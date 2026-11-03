package qoder

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/safe"
)

func collectQoderLines(t *testing.T, body io.Reader) ([]string, error) {
	t.Helper()
	var lines []string
	err := readQoderStream(context.Background(), body, func(line string) bool {
		lines = append(lines, line)
		return true
	})
	return lines, err
}

// 以前单行上限 1 MiB：一行 2 MiB 的工具调用参数会让整条流以 bufio.ErrTooLong 中断。
func TestReadQoderStreamAcceptsMultiMegabyteLine(t *testing.T) {
	long := "data: {\"arguments\":\"" + strings.Repeat("x", 2*1024*1024) + "\"}"
	body := strings.NewReader("data: first\n\n" + long + "\n\ndata: [DONE]\n")
	lines, err := collectQoderLines(t, body)
	if err != nil {
		t.Fatalf("2 MiB line must be read, got %v", err)
	}
	if len(lines) != 3 || lines[0] != "data: first" || lines[1] != long || lines[2] != "data: [DONE]" {
		t.Fatalf("unexpected lines: count=%d first=%q", len(lines), lines[0])
	}
}

// 上限与 hostbridge.SSEScannerBuffer 一致：超过它仍然报 ErrTooLong，而不是无限制吃内存。
func TestReadQoderStreamLimitMatchesSharedBuffer(t *testing.T) {
	body := strings.NewReader("data: " + strings.Repeat("y", hostbridge.SSEScannerBuffer) + "\n")
	_, err := collectQoderLines(t, body)
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("line over SSEScannerBuffer must fail with ErrTooLong, got %v", err)
	}
}

func TestReadQoderStreamStopsWhenConsumerDeclines(t *testing.T) {
	body := strings.NewReader("data: a\ndata: b\ndata: c\n")
	var seen []string
	err := readQoderStream(context.Background(), body, func(line string) bool {
		seen = append(seen, line)
		return false
	})
	if err != nil || len(seen) != 1 {
		t.Fatalf("want stop after first line, got %v %v", seen, err)
	}
}

// panicReader 在读取时 panic，模拟读流协程里的意外。
type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("reader exploded") }

func TestReadQoderStreamConvertsReaderPanicToError(t *testing.T) {
	_, err := collectQoderLines(t, panicReader{})
	var panicErr *safe.PanicError
	if !errors.As(err, &panicErr) {
		t.Fatalf("reader panic must become *safe.PanicError, got %T %v", err, err)
	}
}
