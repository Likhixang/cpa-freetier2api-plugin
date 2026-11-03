package hostbridge

// 本文件覆盖宿主 RPC 通道的契约：回调 ID 绑定、未注入时的显式报错、
// 注入后的调用转发与并发计数。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestCallbackIDBinding(t *testing.T) {
	ctx := WithCallbackID(context.TODO(), "cb-1")
	if got := CallbackID(ctx); got != "cb-1" {
		t.Fatalf("callback id = %q, want cb-1", got)
	}
	// 空 ID 不写入 context。
	if got := CallbackID(WithCallbackID(context.TODO(), "   ")); got != "" {
		t.Fatalf("blank callback id must not be stored, got %q", got)
	}
}

// TestCallbackIDToleratesNilContext 锁定生产代码对 nil context 的兼容处理，所以刻意传 nil。
func TestCallbackIDToleratesNilContext(t *testing.T) {
	//lint:ignore SA1012 刻意验证 nil context 的兼容处理
	if got := CallbackID(WithCallbackID(nil, "cb-nil")); got != "cb-nil" {
		t.Fatalf("WithCallbackID(nil) must fall back to Background, got %q", got)
	}
	//lint:ignore SA1012 刻意验证 nil context 的兼容处理
	if got := CallbackID(nil); got != "" {
		t.Fatalf("nil context must yield empty id, got %q", got)
	}
}

func TestCallRequiresConfiguredCaller(t *testing.T) {
	Configure(nil)
	if _, errCall := Call("", "host.test", nil); !errors.Is(errCall, ErrNotConfigured) {
		t.Fatalf("unconfigured call error = %v, want ErrNotConfigured", errCall)
	}
}

func TestCallForwardsAndCountsInflight(t *testing.T) {
	var seenCallback, seenMethod string
	var seenPayload any
	Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		seenCallback, seenMethod, seenPayload = callbackID, method, payload
		if Inflight() < 1 {
			t.Fatalf("inflight must be positive during call, got %d", Inflight())
		}
		return json.RawMessage(`{"ok":true}`), nil
	})
	t.Cleanup(func() { Configure(nil) })

	ctx := WithCallbackID(context.TODO(), "cb-2")
	if _, errCall := CallCtx(ctx, "host.test", map[string]any{"k": "v"}); errCall != nil {
		t.Fatalf("call: %v", errCall)
	}
	if seenCallback != "cb-2" || seenMethod != "host.test" {
		t.Fatalf("forwarded = %q/%q", seenCallback, seenMethod)
	}
	if payload, okPayload := seenPayload.(map[string]any); !okPayload || payload["k"] != "v" {
		t.Fatalf("payload = %v", seenPayload)
	}
	if Inflight() != 0 {
		t.Fatalf("inflight after call = %d", Inflight())
	}
}

func TestMarshalPayloadInjectsCallbackID(t *testing.T) {
	raw, errMarshal := MarshalPayload("cb-3", map[string]any{"method": "GET"})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	var fields map[string]any
	if errUnmarshal := json.Unmarshal(raw, &fields); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if fields["host_callback_id"] != "cb-3" || fields["method"] != "GET" {
		t.Fatalf("payload = %v", fields)
	}
}

func TestDecodeEnvelope(t *testing.T) {
	result, errDecode := DecodeEnvelope([]byte(`{"ok":true,"result":{"a":1}}`))
	if errDecode != nil || string(result) != `{"a":1}` {
		t.Fatalf("decode = %s %v", result, errDecode)
	}
	if _, errBad := DecodeEnvelope([]byte(`{"ok":false,"error":{"code":"x","message":"y"}}`)); errBad == nil {
		t.Fatal("failed envelope must return an error")
	}
}
