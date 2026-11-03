package app

// 本文件是 model provider 能力的 ABI 适配层。

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

type staticModelRPCRequest struct {
	pluginapi.StaticModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type authModelRPCRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (a *App) handleModelStatic(request []byte) ([]byte, error) {
	var rpc staticModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := hostbridge.WithCallbackID(context.Background(), rpc.HostCallbackID)
	models, errModels := a.models.StaticModels(ctx)
	if errModels != nil {
		return nil, core.ToPluginError(errModels)
	}
	return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey, Models: models})
}

func (a *App) handleModelRegister(request []byte) ([]byte, error) {
	var rpc staticModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := hostbridge.WithCallbackID(context.Background(), rpc.HostCallbackID)
	models, errModels := a.models.StaticModels(ctx)
	if errModels != nil {
		return nil, core.ToPluginError(errModels)
	}
	return core.OKEnvelope(pluginapi.ModelRegistrationResponse{Provider: providerKey, Models: models})
}

func (a *App) handleModelForAuth(request []byte) ([]byte, error) {
	var rpc authModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := hostbridge.WithCallbackID(context.Background(), rpc.HostCallbackID)
	credential, vendor, errResolve := a.resolveVendorCredential(ctx, rpc.HostCallbackID, rpc.StorageJSON, rpc.AuthID, rpc.Attributes)
	if errResolve != nil {
		logger.Debug("model.for_auth: no credential for %s: %v", rpc.AuthID, errResolve)
		return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	if !a.models.RealmEnabled(vendor.Region()) {
		return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	models, errModels := core.ModelsFor(vendor, ctx, credential)
	probed := errModels == nil
	if !probed {
		// 探测失败 ≠ 该凭证没有模型。进程内模型缓存挂在 Catalog 上，随 App 实例
		// 一起消失，宿主热重载后每个凭证都会现场探测一轮；上游一次抖动（429、
		// 超时、瞬时 5xx）就足以让整家供应商从 /v1/models 上消失，只能手动重存
		// 设置才恢复。这里回落到上一次成功的清单，把「这次没取到」与「凭证真的
		// 不可用」分开——后者由供应商返回空清单或确定性拒绝表达，不走这条回落。
		if cached := a.cachedModelsFor(vendor, errModels); len(cached) > 0 {
			logger.Info("model.for_auth: probe failed for %s (%v); falling back to cached catalog (%d model(s))",
				vendor.ID(), errModels, len(cached))
			models = cached
		} else {
			logger.Debug("model.for_auth: probe models for %s failed and no fallback catalog: %v", vendor.ID(), errModels)
			return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey})
		}
	}
	// 顺手写入控制台回读缓存（官方 ID，未过滤/未换别名）。
	//
	// 宿主加载每个凭证文件都会走这条 RPC，因此插件启动后不再依赖 static 探测
	// 的时序：控制台列表不会因为探测还没跑完而缺整家供应商。
	// 空清单不写缓存：供应商在探测失败时按契约返回空结果（让宿主淘汰坏凭证），
	// 若照样覆盖，一次瞬时失败就会把整家供应商从控制台抹掉。
	// 回落值本身就是缓存，写回去只会多一次落盘，因此只缓存真正探测成功的清单。
	if probed && !a.models.CacheProbedModels(vendor.ID(), models) {
		logger.Info("model.for_auth: %s returned no models for %s (host will drop this credential)",
			vendor.ID(), rpc.AuthID)
	}
	return core.OKEnvelope(pluginapi.ModelResponse{
		Provider: providerKey,
		Models:   a.models.ApplyAliases(vendor.ID(), a.models.FilterForVendor(vendor.ID(), models)),
	})
}

// cachedModelsFor 返回该供应商最近一次成功探测到的清单，取不到时返回空。
//
// 优先用进程内缓存；宿主热重载后新 App 的进程内缓存是空的，此时回落到
// state.json——它是唯一跨实例存活的那份。
//
// 两道闸门决定「什么时候不该回落」：
//   - 确定性失败（凭证被拒、参数错误等不可重试的分类）说明这份凭证真的用不了，
//     回落只会让宿主永远淘汰不掉坏凭证；
//   - 计费通道（core.VetoesModelFallback）的清单本身就是「还能推哪些模型」的
//     判据，探测失败时沿用等于继续推广可能已无权限的模型。
func (a *App) cachedModelsFor(vendor core.Vendor, probeErr error) []pluginapi.ModelInfo {
	if core.VetoesModelFallback(vendor) || !modelProbeFallbackAllowed(probeErr) {
		return nil
	}
	if cached := a.models.Registered(vendor.ID()); len(cached) > 0 {
		return cached
	}
	return modelsFromState(a.SnapshotState().Models, vendor.ID())
}

// modelProbeFallbackAllowed 报告一次探测失败是否属于「上游暂时不可用」。
//
// 429、5xx、账号级故障与传输层错误都说明清单本身没变，只是这次没取到；而
// 401/403 这类确定性拒绝说明凭证已被上游作废，必须如实报空让宿主处置。
func modelProbeFallbackAllowed(probeErr error) bool {
	var upstreamErr core.UpstreamError
	if errors.As(probeErr, &upstreamErr) {
		return upstreamErr.Retryable()
	}
	// 未分类错误来自本地或传输层（超时、连接中断、编解码），清单没有变化。
	return true
}

// modelsFromState 从 state.json 的模型缓存里挑出某供应商的条目。
//
// 只认显式带 vendor_id 的条目：更早的版本按 realm 落盘、由 admin 侧做兼容映射，
// 这里按 realm 猜供应商会把一个渠道的模型记到另一个渠道名下。
func modelsFromState(cached []core.CachedModel, vendorID string) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(cached))
	for _, item := range cached {
		if !strings.EqualFold(strings.TrimSpace(item.VendorID), vendorID) || strings.TrimSpace(item.ID) == "" {
			continue
		}
		out = append(out, core.ModelInfoToPluginAPI(vendorID, core.ModelDescriptor{
			ID:              item.ID,
			Name:            item.Name,
			Description:     item.Description,
			ContextLength:   item.ContextLength,
			MaxOutputTokens: item.MaxOutputTokens,
			SupportsImages:  item.SupportsImages,
			SupportsTools:   item.SupportsTool,
		}))
	}
	return out
}

func (a *App) applyModelCatalog(_ pluginConfig) {
	a.models.ClearRestartPending()
	snapshot := a.SnapshotState()
	a.models.ReloadFromState(snapshot)
	a.models.LoadCachedState(snapshot.Models)
}

func (a *App) refreshModelsFromUpstream(ctx context.Context) (int, error) {
	total, errRefresh := a.models.RefreshFromUpstream(ctx)
	if errRefresh != nil {
		return 0, core.ToPluginError(errRefresh)
	}
	return total, nil
}

func (a *App) resolveOutboundModel(vendorID, model string) string {
	return a.models.ResolveOutboundModel(vendorID, model)
}

func (a *App) applyModelAliases(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	return a.models.ApplyAliases(vendorID, models)
}

func (a *App) isModelDisabled(vendorID, modelID string) bool {
	return a.models.IsDisabled(vendorID, modelID)
}
