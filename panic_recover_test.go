package main

// 宿主 RPC 入口的 panic 兜底：插件是宿主进程里的动态库，入口处放走的 panic
// 会让整个 CPA 退出。这里锁定「panic → internal_error 错误响应」的约定。

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestRecoverMethodConvertsPanicToInternalError(t *testing.T) {
	raw, err := recoverMethod("executor.execute", func() ([]byte, error) {
		var values []int
		_ = values[3] // 越界：运行时 panic
		return []byte("unreachable"), nil
	})
	if raw != nil {
		t.Fatalf("raw must be nil after panic, got %q", raw)
	}
	var pluginErr *core.PluginError
	if !errors.As(err, &pluginErr) {
		t.Fatalf("want *core.PluginError, got %T %v", err, err)
	}
	if pluginErr.Code != internalErrorCode || pluginErr.HTTPStatus != http.StatusInternalServerError || pluginErr.Retryable {
		t.Fatalf("unexpected plugin error: %+v", pluginErr)
	}
	if !strings.Contains(pluginErr.Message, "rpc executor.execute") || !strings.Contains(pluginErr.Message, "index out of range") {
		t.Fatalf("message should name the method and the panic: %q", pluginErr.Message)
	}

	// 写给宿主的信封带上同一个错误码。
	var envelope struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if errDecode := json.Unmarshal(core.ErrorEnvelopeFromError(err), &envelope); errDecode != nil {
		t.Fatal(errDecode)
	}
	if envelope.OK || envelope.Error.Code != internalErrorCode {
		t.Fatalf("unexpected envelope: %+v", envelope)
	}
}

func TestRecoverMethodKeepsNormalResults(t *testing.T) {
	raw, err := recoverMethod("plugin.info", func() ([]byte, error) { return []byte("ok"), nil })
	if err != nil || string(raw) != "ok" {
		t.Fatalf("got %q %v", raw, err)
	}
	sentinel := core.NewPluginError("bad_request", "nope", http.StatusBadRequest)
	if _, err := recoverMethod("plugin.info", func() ([]byte, error) { return nil, sentinel }); err != sentinel {
		t.Fatalf("business error must pass through unchanged, got %v", err)
	}
}

// TestHandleMethodSafeRoutesThroughRecover 确认真实入口用的是带兜底的版本，且正常请求不受影响。
func TestHandleMethodSafeRoutesThroughRecover(t *testing.T) {
	want, errWant := handleMethod("plugin.info", nil)
	got, errGot := handleMethodSafe("plugin.info", nil)
	if (errWant == nil) != (errGot == nil) || string(want) != string(got) {
		t.Fatalf("handleMethodSafe diverged: %q/%v vs %q/%v", got, errGot, want, errWant)
	}
}
