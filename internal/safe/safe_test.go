package safe

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"freetier2api-plugin/internal/logger"
)

func TestCallConvertsPanicToError(t *testing.T) {
	err := Call("unit", func() error {
		var counters map[string]int
		increment(counters, "boom") // 向 nil map 写入：运行时 panic
		return nil
	})
	var panicErr *PanicError
	if !errors.As(err, &panicErr) {
		t.Fatalf("want *PanicError, got %T %v", err, err)
	}
	if panicErr.Scope != "unit" || len(panicErr.Stack) == 0 {
		t.Fatalf("scope/stack not recorded: %+v", panicErr)
	}
	if !strings.Contains(err.Error(), "internal error in unit") {
		t.Fatalf("unexpected message: %v", err)
	}
}

func TestCallPassesThroughNormalResults(t *testing.T) {
	if err := Call("ok", func() error { return nil }); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
	sentinel := errors.New("plain")
	if err := Call("plain", func() error { return sentinel }); err != sentinel {
		t.Fatalf("own error must be returned as-is, got %v", err)
	}
}

func TestPanicErrorUnwrapsErrorValue(t *testing.T) {
	sentinel := errors.New("cause")
	err := Call("wrap", func() error { panic(sentinel) })
	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is should reach the panic value, got %v", err)
	}
}

func TestGoRecoversAndLogs(t *testing.T) {
	_, since := logger.Snapshot(0, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	Go("background-unit", func() {
		defer wg.Done()
		panic("kaboom")
	})
	wg.Wait()
	// 进程还活着本身就证明 panic 被挡住了；再确认留下了可排查的日志。
	// fn 自己的 defer 先于 Recover 执行，所以这里短暂轮询日志。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := logger.Snapshot(since, 50)
		for _, entry := range entries {
			if strings.Contains(entry.Message, "recovered panic in background-unit: kaboom") {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("panic log not found")
}

func increment(counters map[string]int, key string) { counters[key]++ }
