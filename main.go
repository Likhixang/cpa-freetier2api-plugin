package main

// 本文件只保留 c-shared 必需的入口、版本注入与 ABI 转接。
// 领域逻辑位于 internal/app；C ABI 在 cgo.go。

import (
	"net/http"

	"freetier2api-plugin/internal/app"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/safe"
)

// pluginVersion 由 CI 通过 -ldflags -X main.pluginVersion=<tag> 注入。
var pluginVersion = app.DefaultVersion

var pluginApp = app.MustNew(app.Options{
	Version:   pluginVersion,
	Factories: app.BuiltinFactories(),
})

// internalErrorCode 是插件内部 panic 转成的错误码。
const internalErrorCode = "internal_error"

// handleMethod 保留给 cgo.go 与历史测试调用。
func handleMethod(method string, request []byte) ([]byte, error) {
	return pluginApp.HandleMethod(method, request)
}

// handleMethodSafe 是 ABI 入口实际调用的版本：处理过程中的 panic 转成
// internal_error（HTTP 500）错误响应，只让这一次请求失败，而不是拖垮宿主进程。
func handleMethodSafe(method string, request []byte) ([]byte, error) {
	return recoverMethod(method, func() ([]byte, error) { return handleMethod(method, request) })
}

// recoverMethod 执行 handle，把其中的 panic 转成插件错误。单独拆出来便于测试。
func recoverMethod(method string, handle func() ([]byte, error)) (raw []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			panicErr := safe.NewPanicError("rpc "+method, r)
			raw, err = nil, core.NewPluginError(internalErrorCode, panicErr.Error(), http.StatusInternalServerError)
		}
	}()
	return handle()
}
