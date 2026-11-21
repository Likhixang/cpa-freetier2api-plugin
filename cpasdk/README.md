# cpasdk

本目录是 CLIProxyAPI 宿主 ABI/JSON 契约的 **vendored 副本**，不是第三方依赖：

- `pluginabi/types.go` 对齐宿主 `sdk/pluginabi/types.go`；
- `pluginapi/types.go` 对齐宿主 `sdk/pluginapi/types.go`。

**当前对齐版本：CLIProxyAPI v8.0.80（ABIVersion 1 / SchemaVersion 10）。**

同步方式：从宿主对应版本复制这两个文件，保持排版一致；`gofmt` 检查刻意排除本目录。
宿主升级 ABI 时先更新这里，再调整 `internal/app` 的 RPC 适配层。

## 与上游的两处刻意偏离

1. **导入用标准库 `encoding/json`**：宿主从 v8.0.7x 起改用 `github.com/MoeForge/moego/json`，
   本插件保持零第三方依赖，只改导入行，类型定义与上游逐字一致。
2. **本地扩展字段**（宿主解码时忽略，只有本插件自己消费，同步上游时不要删）：
   - `pluginapi.QuotaFetchResponse.Claim` / `pluginapi.ClaimStatus`：活动套餐领取状态；
   - `pluginapi.ManagementRequest.Context` / `RequestContext()`：管理请求生命周期。

## 宿主请求缓冲区契约（`cliproxy_plugin_api.call` 的 `request`）

- 宿主侧约定：`request` / `request_len` 指向的内存在**本次同步调用期间**有效，且不会被并发改写；
  调用返回后插件不得再访问它。这条是宿主侧承诺（宿主源码不在本仓库）。
- 插件侧默认用 `C.GoBytes` 复制一份再处理。
- 例外：`response.normalize_before` 在根目录 `cgo.go` 的 `hostRequestBytes` +
  `isReadOnlyRequestMethod` 里直接借用宿主缓冲区（每个流式分片都会带着两份完整请求体，
  复制开销与请求大小成正比）。
- 因此进入该名单的方法必须满足：**只读**（不修改入参）、**不持有**（不保存任何切片片段）、
  **不异步**（不在调用返回后的 goroutine 里访问）。新增方法前先确认这三条。
