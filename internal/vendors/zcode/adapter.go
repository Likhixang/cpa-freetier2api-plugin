package zcode

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// 本文件把 ZCode 适配成 core.Vendor。
//
// 凭证形态是 oauth：账号由控制台发起 OAuth 登录产生（一次拿到 Coding Plan
// JWT 与兑换出的 API Key）。之所以声明为 oauth 而不是 apikey，是因为页面按
// auth_mode 决定「添加账号」的动作——apikey 会直接弹输入框、不发起登录。
//
// 通道选择：具备 JWT 时优先走 Start Plan 通道（zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages）；
// 纯 API Key 账号走 ZCode 官方网关转发通道（zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages）。

// 供应商配置键。
const (
	// configKeyBaseURLCN / configKeyBaseURLGlobal 是实例级上游基地址覆盖，
	// 与 minimaxcode 的按实例拆键保持一致：同名键会被每个实例各报一次。
	configKeyBaseURLCN     = "zcodecn_base_url"
	configKeyBaseURLGlobal = "zcodeglobal_base_url"
	// configKeyBaseURLLegacy 是拆分前的族级键，仅作兼容读取（不上报）。
	configKeyBaseURLLegacy = "zcode_base_url"
	// configKeyClaimMinInterval 是套餐领取节流间隔（族级键，两个实例共读）。
	configKeyClaimMinInterval = "zcode_claim_min_interval"
)

// adapter 是 core.Vendor 在 ZCode 上的实现。
type adapter struct {
	*core.NativeAdapter[*Credential]
	// vendorID 是实例标识（国内版 zcodecn / 国际版 zcodeglobal）。
	vendorID string
	// region / provider 固定本实例的登录来源。
	region   string
	provider string
	baseURL  string
	// claims 是本实例的领取节流器；不共享，多 App 并存时互不影响。
	claims *claimGate
	// activations 是本实例的活跃上报去重表（每账号每日一次）。
	activations *activationState
	// gate 是本实例的 Start Plan 出站闸门；不共享，多 App 并存时互不影响。
	gate *chatGate
}

// Instances 构造 ZCode 国内 / 国际两个实例。
//
// 两者协议与上游通道相同，区别只在登录 provider 与凭证归属：
//   - 国内版 zcodecn：bigmodel（智谱开放平台）
//   - 国际版 zcodeglobal：zai（z.ai）
func Instances(env core.Env) []core.Vendor {
	return []core.Vendor{
		newInstance(env, VendorIDCN, VendorNameCN, RegionCN, ProviderBigmodel),
		newInstance(env, VendorIDGlobal, VendorNameGlobal, RegionGlobal, ProviderZai),
	}
}

func newInstance(env core.Env, vendorID, vendorName, region, provider string) *adapter {
	instance := &adapter{
		vendorID:    vendorID,
		region:      region,
		provider:    provider,
		claims:      newClaimGate(),
		activations: newActivationState(),
		gate:        newChatGate(),
	}
	instance.NativeAdapter = core.NewNativeAdapter(core.NativeSpec[*Credential]{
		Meta: core.Meta{VendorID: vendorID, VendorName: vendorName, VendorRegion: region},
		Env:  env,
		Extract: func(c *core.Credential) (*Credential, bool) {
			native, ok := c.Native.(*Credential)
			return native, ok
		},
		ParseSeed: func(raw []byte, fileName string) (core.CredentialSeed, *Credential, error) {
			native, errParse := ParseCredential(raw)
			if errParse != nil {
				return core.CredentialSeed{}, nil, errParse
			}
			// 旧凭证（vendor=zcode、无 region）按本实例补齐并迁移到新 ID。
			native.Region = core.FirstNonEmpty(native.Region, region)
			native.Provider = core.FirstNonEmpty(native.Provider, provider)
			native.Vendor = vendorID
			label := native.Label
			if label == "" {
				label = core.FirstNonEmpty(native.Email, "ZCode-"+MaskedKey(native.OutboundToken()))
			}
			return core.CredentialSeed{
				VendorID: vendorID, Region: native.Region, Label: label,
				FileID: core.FileIDFromFileName(fileName), UID: core.FirstNonEmpty(native.UserID, native.Email),
				Token: native.APIKey, AuthMode: native.AuthMode,
			}, native, nil
		},
		MergeNative: func(original []byte, native *Credential) ([]byte, error) {
			return MergeStorageJSON(original, native)
		},
	})
	return instance
}

// AuthMode 返回凭证形态：oauth。
func (v *adapter) AuthMode() string { return "oauth" }

// Configure 解析供应商专属配置。
//
// 基地址按实例拆键：两个实例报同一个键会让宿主配置表单出现两条同名项。
// 旧族级键 configKeyBaseURLLegacy 两个实例都读，升级后行为不变。
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	v.baseURL = strings.TrimSpace(core.FirstNonEmpty(raw[v.baseURLKey()], raw[configKeyBaseURLLegacy]))
	if interval, errParse := time.ParseDuration(strings.TrimSpace(raw[configKeyClaimMinInterval])); errParse == nil && interval >= 0 {
		v.claims.setInterval(interval)
	}
}

// baseURLKey 返回本实例的上游基地址覆盖键。
func (v *adapter) baseURLKey() string {
	if v.vendorID == VendorIDCN {
		return configKeyBaseURLCN
	}
	return configKeyBaseURLGlobal
}

// Match 判断一份凭证是否属于本实例（归属规则见 LooksLikeCredential）。
func (v *adapter) Match(fileName, _ string, raw map[string]any) bool {
	return LooksLikeCredential(raw, fileName, v.region)
}

// LegacyIDs 声明拆分前的旧 supplier ID。
//
// 拆分前只有国际版（provider 恒为 zai），因此旧前缀 zcode 只对应国际版：
// 用户在 disabled_models / 模型别名里写的 zcode:<模型> 继续落到本实例。
func (v *adapter) LegacyIDs() []string {
	if v.vendorID != VendorIDGlobal {
		return nil
	}
	return []string{VendorID}
}

// ---- 模型 ----

// StaticModels 返回模型清单。
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := FetchModels(ctx)
	if errModels != nil {
		return nil, errModels
	}
	return modelsToPluginAPI(models), nil
}
func modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	return core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		if strings.TrimSpace(model.ID) == "" {
			return core.ModelDescriptor{}, false
		}
		return core.ModelDescriptor{
			ID:                 model.ID,
			Name:               model.Name,
			Description:        model.Description,
			ContextLength:      int64(model.ContextWindow),
			MaxOutputTokens:    int64(model.MaxTokens),
			SupportsTools:      model.SupportsTools,
			SupportsImages:     model.SupportsImages,
			CanDisableThinking: !model.IsReasoning,
		}, true
	})
}

// ---- 执行 ----

// Execute 执行一次非流式对话。
func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	release, errGate := v.holdStartPlanSlot(ctx, native)
	if errGate != nil {
		return nil, errGate
	}
	defer release()

	payload, errChat := Chat(ctx, v.baseURL, ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Stream:     false,
	})
	if errChat != nil {
		return nil, errChat
	}
	return &pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

// ExecuteStream 执行一次流式对话。
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	release, errGate := v.holdStartPlanSlot(ctx, native)
	if errGate != nil {
		return errGate
	}
	defer release()

	return ChatStream(ctx, v.baseURL, ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Stream:     true,
	}, func(chunk []byte) error {
		return sink.Emit(chunk)
	})
}

// ---- 额度与签到 ----

// Quota 查询账号额度。
func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	return v.fetchQuota(ctx, native)
}

// ---- 登录与续期 ----

// LoginStart 发起一次登录（本实例固定的国内 / 国际 provider）。
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return LoginStart(ctx, v.Env.Logins, v.vendorID, v.region, v.provider)
}

// LoginPoll 轮询一次登录状态。
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return LoginPoll(ctx, v.Env.Logins, state)
}

// ConfigFields 声明 zcode 专属配置。
//
// 领取节流是族级语义（两个实例读同一个键），只由一个实例上报，否则宿主
// 配置表单会出现同名重复项。
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	fields := []pluginapi.ConfigField{
		{Name: v.baseURLKey(), Type: pluginapi.ConfigFieldTypeString, Description: "上游基地址覆盖（仅对话通道，OAuth 固定直连 zcode.z.ai）。"},
	}
	if v.vendorID == VendorIDGlobal {
		fields = append(fields, pluginapi.ConfigField{
			Name: configKeyClaimMinInterval, Type: pluginapi.ConfigFieldTypeString,
			Description: "两次套餐领取之间的最小间隔（Go 时长，如 15s；0 表示不节流）。",
		})
	}
	return fields
}

// ---- 一键任务 ----

// 一键任务里代表各自动作的任务码。
const (
	// AutoTaskCodeActivity 是「活动资格」：上报 app_launch / app_daily_active。
	AutoTaskCodeActivity = "(活动资格)"
	// AutoTaskCodePlanClaim 是「套餐领取」：列出可领套餐，但领取需要人机验证。
	AutoTaskCodePlanClaim = "(套餐领取)"
)

// autoTask 是一键任务表的一行。
type autoTask struct {
	code  string
	label string
	// skip 返回非空原因时本行不执行（→ skipped，原因即此）。
	skip func(*autoRun) string
	// exec 执行本行；outcome.message 为空表示无事可做（原因见 reason）。
	exec func(context.Context, *autoRun) (autoOutcome, error)
}

// autoOutcome 是一行任务的产出。
type autoOutcome struct {
	message string
	reason  string
	credit  int64
	energy  int64
}

// autoRun 是一轮一键任务的共享上下文。
type autoRun struct {
	vendor *adapter
	cred   *Credential
}

// autoTasks 是一键任务表（顺序即依赖序：先报资格，再看投放）。
//
// ZCode 的活动套餐领取必须带人机验证令牌（core.PlanClaimRequest.VerifyParam
// 由控制台的验证码组件产出），代理侧没有可替代的求解手段，所以「套餐领取」
// 这一行只做能自动完成的那一半：列出当前可领数量，并如实跳过。
var autoTasks = []autoTask{
	{
		code:  AutoTaskCodeActivity,
		label: "活动资格",
		skip: func(run *autoRun) string {
			if run.cred == nil {
				return "凭证缺失"
			}
			return ""
		},
		exec: func(ctx context.Context, run *autoRun) (autoOutcome, error) {
			probe, errProbe := run.vendor.probeActivity(ctx, run.cred)
			if errProbe != nil {
				return autoOutcome{}, errProbe
			}
			message := "已上报活动资格事件（app_launch / app_daily_active）"
			if probe.Warning != "" {
				message += "（" + probe.Warning + "）"
			}
			return autoOutcome{message: message}, nil
		},
	},
	{
		code:  AutoTaskCodePlanClaim,
		label: "套餐领取",
		skip: func(run *autoRun) string {
			return billingBlockReason(run.cred)
		},
		exec: func(ctx context.Context, run *autoRun) (autoOutcome, error) {
			plans, errPreview := run.vendor.previewPlans(ctx, run.cred)
			if errPreview != nil {
				return autoOutcome{}, errPreview
			}
			if len(plans) == 0 {
				return autoOutcome{reason: "当前没有可领的套餐（已领完或活动未投放）"}, nil
			}
			return autoOutcome{
				reason: "当前 " + strconv.Itoa(len(plans)) + " 个套餐可领，领取需在控制台输入人机验证码",
			}, nil
		},
	},
}

// runAutoTasks 按表执行，把每一行折成 core 的三态结果。
func runAutoTasks(ctx context.Context, tasks []autoTask, run *autoRun) []core.AutoTaskResult {
	results := make([]core.AutoTaskResult, 0, len(tasks))
	for _, task := range tasks {
		results = append(results, task.run(ctx, run))
	}
	return results
}

// run 执行一行任务：前置不成立即 skipped，执行失败即 error，无产出即 skipped。
func (t autoTask) run(ctx context.Context, run *autoRun) core.AutoTaskResult {
	result := core.AutoTaskResult{Code: t.code}
	if t.skip != nil {
		if reason := t.skip(run); reason != "" {
			result.Status = core.AutoStatusSkipped
			result.Message = reason
			return result
		}
	}
	outcome, errExec := t.exec(ctx, run)
	result.Credit, result.Energy = outcome.credit, outcome.energy
	switch {
	case errExec != nil:
		result.Status = core.AutoStatusError
		result.Message = t.label + "：" + failureReason(errExec)
	case strings.TrimSpace(outcome.message) == "":
		result.Status = core.AutoStatusSkipped
		result.Message = core.FirstNonEmpty(outcome.reason, "当前无需动作")
	default:
		result.Status = core.AutoStatusDone
		result.Message = outcome.message
	}
	return result
}

// autoTaskFor 按任务码查表。
func autoTaskFor(code string) (autoTask, bool) {
	trimmed := strings.TrimSpace(code)
	for _, task := range autoTasks {
		if task.code == trimmed {
			return task, true
		}
	}
	return autoTask{}, false
}

// failureReason 取用户可读的失败原因：分类错误优先用上游原文，避免把
// "upstream waf_block [403]:" 这类内部分类前缀怼到页面上。
func failureReason(err error) string {
	var failure *core.Failure
	if errors.As(err, &failure) && strings.TrimSpace(failure.Msg) != "" {
		return failure.Msg
	}
	return err.Error()
}

// RunAllTasks 执行一键任务。
func (v *adapter) RunAllTasks(ctx context.Context, cred *core.Credential) []core.AutoTaskResult {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return []core.AutoTaskResult{{
			Code: AutoTaskCodeActivity, Status: core.AutoStatusError, Message: "账号：" + errNative.Error(),
		}}
	}
	return runAutoTasks(ctx, autoTasks, &autoRun{vendor: v, cred: native})
}

// RunTask 执行单条任务；未知任务码如实跳过。
func (v *adapter) RunTask(ctx context.Context, cred *core.Credential, code string) (core.TaskRunResult, error) {
	task, okTask := autoTaskFor(code)
	if !okTask {
		return core.TaskRunResult{Status: core.AutoStatusSkipped, Message: "该供应商没有这条任务"}, nil
	}
	native, errNative := v.Native(cred)
	if errNative != nil {
		return core.TaskRunResult{}, errNative
	}
	row := task.run(ctx, &autoRun{vendor: v, cred: native})
	return core.TaskRunResult{Status: row.Status, Message: row.Message, Credit: row.Credit, Energy: row.Energy}, nil
}

// 编译期断言：适配器实际具备的供应商能力。
var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.AuthModeReporter     = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.LoginProvider        = (*adapter)(nil)
	_ core.QuotaProvider        = (*adapter)(nil)
	_ core.Refresher            = (*adapter)(nil)
	_ core.ActivityProbe        = (*adapter)(nil)
	_ core.PlanClaimer          = (*adapter)(nil)
	_ core.LegacyIDReporter     = (*adapter)(nil)
	_ core.TaskRunner           = (*adapter)(nil)
)
