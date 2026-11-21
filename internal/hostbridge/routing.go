package hostbridge

// 本文件封装宿主的冷却重置回调（host.routing.reset_cooldown）。
//
// 场景：免费额度按周期重置（签到 / 额度回补），而宿主侧的冷却与额度状态还挂在
// 这条凭证上。不主动清掉，宿主会继续跳过它，直到冷却自然过期。

import (
	"encoding/json"
	"strings"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
)

// ResetRoutingCooldown 请宿主清掉一条凭证的额度与冷却路由状态。
//
// 只清路由状态，不动凭证文件；authIndex 为空时直接返回空结果（没有可清的对象）。
// 宿主在 v8.0.x 之前没有这个方法，回调失败由调用方降级处理（记日志即可）：
// 冷却重置是尽力而为的优化，不能因为它阻断签到结果。
func ResetRoutingCooldown(callbackID, authIndex string) (pluginapi.HostRoutingResetCooldownResponse, error) {
	trimmed := strings.TrimSpace(authIndex)
	if trimmed == "" {
		return pluginapi.HostRoutingResetCooldownResponse{}, nil
	}
	raw, errCall := Call(callbackID, pluginabi.MethodHostRoutingResetCooldown,
		pluginapi.HostRoutingResetCooldownRequest{AuthIndex: trimmed})
	if errCall != nil {
		return pluginapi.HostRoutingResetCooldownResponse{}, errCall
	}
	var resp pluginapi.HostRoutingResetCooldownResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return pluginapi.HostRoutingResetCooldownResponse{}, errDecode
	}
	return resp, nil
}
