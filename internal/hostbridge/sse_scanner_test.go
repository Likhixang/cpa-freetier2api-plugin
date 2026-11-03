package hostbridge

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

// NewSSEScanner 的上限就是 SSEScannerBuffer：多 MB 的单行照常读出，超过上限报 ErrTooLong。
func TestNewSSEScannerLimit(t *testing.T) {
	long := "data: " + strings.Repeat("z", 5*1024*1024)
	scanner := NewSSEScanner(strings.NewReader("data: a\n" + long + "\r\n\ndata: b"))
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if errScan := scanner.Err(); errScan != nil {
		t.Fatalf("5 MiB line must be accepted, got %v", errScan)
	}
	// 与 bufio.ScanLines 语义一致：去掉 \r\n，保留空行，最后一行无换行也照常返回。
	if len(lines) != 4 || lines[0] != "data: a" || lines[1] != long || lines[2] != "" || lines[3] != "data: b" {
		t.Fatalf("unexpected lines: count=%d", len(lines))
	}

	over := NewSSEScanner(strings.NewReader(strings.Repeat("z", SSEScannerBuffer+1) + "\n"))
	for over.Scan() {
	}
	if !errors.Is(over.Err(), bufio.ErrTooLong) {
		t.Fatalf("line over SSEScannerBuffer must fail with ErrTooLong, got %v", over.Err())
	}
}
