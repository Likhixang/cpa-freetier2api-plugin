package app

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"freetier2api-plugin/internal/core"
)

// panicStreamVendor 在流式执行里 panic，模拟解析上游响应时的空指针一类故障。
type panicStreamVendor struct{ *writeStubVendor }

func (panicStreamVendor) ExecuteStream(context.Context, *core.Credential, *core.ExecuteRequest, core.StreamSink) error {
	var frame *struct{ Data string }
	_ = frame.Data // nil 解引用：运行时 panic
	return nil
}

func TestExecuteStreamSafeConvertsVendorPanic(t *testing.T) {
	prepared := preparedExecution{
		vendor:     panicStreamVendor{&writeStubVendor{}},
		credential: &core.Credential{},
	}
	prepared.rpc.StreamID = "stream-1"

	err := executeStreamSafe(context.Background(), prepared, nil)
	var pluginErr *core.PluginError
	if !errors.As(err, &pluginErr) {
		t.Fatalf("want *core.PluginError, got %T %v", err, err)
	}
	// 内部错误不该让宿主换号重试：换哪个号都会触发同一处代码。
	if pluginErr.Code != "internal_error" || pluginErr.HTTPStatus != http.StatusInternalServerError || pluginErr.Retryable {
		t.Fatalf("unexpected plugin error: %+v", pluginErr)
	}
}

func TestExecuteStreamSafeKeepsVendorError(t *testing.T) {
	prepared := preparedExecution{vendor: &writeStubVendor{}, credential: &core.Credential{}}
	err := executeStreamSafe(context.Background(), prepared, nil)
	if err == nil || err.Error() != "not used in this test" {
		t.Fatalf("vendor error must pass through unchanged, got %v", err)
	}
}
