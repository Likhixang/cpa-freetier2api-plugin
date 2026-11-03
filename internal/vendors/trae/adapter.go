package trae

// 本文件把 Trae 国内版与 Trae 国际版适配成 core.Vendor。
//
// 两个实例共享同一套协议实现，差异由 Config/profile 描述。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

type adapter struct {
	*core.NativeAdapter[*Credential]
	// config 是构造期的 profile，只读。
	config Config
	// ideVersion 是 IDE 版本号的配置覆盖；空串表示沿用 config 的编译期默认值。
	// 两个实例的版本线不同（CN / global 各自是 3.3.x），所以配置键按实例 ID 区分
	// （见 Configure），不能共用族级键。
	ideVersionMu   sync.RWMutex
	ideVersion     string
	ideVersionCode string
	sessions       *SessionStore
	// catalog 是模型目录缓存（按账号），随实例隔离。
	catalog catalogCache
	// credits 缓存最近一轮目录探测的倍率表（官方模型 ID → 展示文案）。
	credits core.CreditTable
}

// profile 返回当前 profile 快照（编译期 profile + 运行期版本覆盖）。
//
// 版本号决定上游下发的模型目录，可在运行期改写，所以所有读取点都必须走这里。
func (v *adapter) profile() Config {
	v.ideVersionMu.RLock()
	defer v.ideVersionMu.RUnlock()
	cfg := v.config
	if v.ideVersion != "" {
		cfg.IDEVersion = v.ideVersion
	}
	if v.ideVersionCode != "" {
		cfg.IDEVersionCode = v.ideVersionCode
	}
	return cfg
}

// Instances 构造 Trae 家族的全部实例。
func Instances(env core.Env) []core.Vendor {
	configs := AllConfigs()
	sessions := NewSessionStore()
	out := make([]core.Vendor, 0, len(configs))
	for _, cfg := range configs {
		out = append(out, newInstance(env, cfg, sessions))
	}
	return out
}

// newInstance 构造一个 region 实例。
//
// profile 的完整性（额度端点、主机、版本号）由 ConfigFor 统一保证，并由
// TestAllConfigsAreComplete 在测试期锁住——构造函数里不留 panic：供应商构造
// 失败会把整个插件启动打挂，代价远高于一个可测的静态断言。
func newInstance(env core.Env, cfg Config, sessions *SessionStore) core.Vendor {
	return &adapter{
		NativeAdapter: core.NewNativeAdapter(core.NativeSpec[*Credential]{
			Meta: core.Meta{VendorID: cfg.VendorID, VendorName: cfg.VendorName, VendorRegion: string(cfg.Region)},
			Env:  env,
			Extract: func(c *core.Credential) (*Credential, bool) {
				native, ok := c.Native.(*Credential)
				return native, ok
			},
			ParseSeed: func(raw []byte, fileName string) (core.CredentialSeed, *Credential, error) {
				native, errParse := ParseCredentialFor(raw, cfg)
				if errParse != nil {
					return core.CredentialSeed{}, nil, errParse
				}
				native.Vendor = cfg.VendorID
				label := native.Label
				if label == "" {
					prefix := "TraeCN-"
					if cfg.Region == RegionGlobal {
						prefix = "TraeGlobal-"
					}
					if native.Nickname != "" {
						label = prefix + native.Nickname
					} else if native.UID != "" {
						label = prefix + native.UID
					} else {
						label = prefix + "Account"
					}
				}
				return core.CredentialSeed{
					VendorID: cfg.VendorID, Region: string(cfg.Region), Label: label,
					FileID: core.FileIDFromFileName(fileName), Token: native.AccessToken,
					ExpiresAt: native.ExpiresAt, AuthMode: "oauth",
				}, native, nil
			},
			RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
				updated, refreshed, errRefresh := RefreshCredential(ctx, native, cfg)
				if errRefresh != nil {
					return core.NativeRefresh[*Credential]{}, errRefresh
				}
				return core.NativeRefresh[*Credential]{
					Native: updated, Token: updated.AccessToken, RefreshToken: updated.RefreshToken,
					ExpiresAt: updated.ExpiresAt, Changed: refreshed,
				}, nil
			},
			MergeNative: func(original []byte, native *Credential) ([]byte, error) {
				return MergeStorageJSON(original, native)
			},
		}),
		config: cfg, sessions: sessions,
	}
}

func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredentialFor(raw, fileName, provider, v.profile())
}

// Configure 解析 Trae 专属配置。
//
// 上游按 X-Ide-Version-Code 下发不同模型目录：国内版构建号低于 20260806 时
// 拿不到 deepseek-v4.1-flash。门控再抬高时用户改配置即可，不必等发版。
// 键名按实例加前缀（traecn_ide_version / traeglobal_ide_version / …），
// 因为三条产品线的版本号体系不同，共用一个键会互相踩。
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	prefix := v.config.VendorID + "_"
	version := strings.TrimSpace(raw[prefix+"ide_version"])
	versionCode := strings.TrimSpace(raw[prefix+"ide_version_code"])
	v.ideVersionMu.Lock()
	v.ideVersion = version
	v.ideVersionCode = versionCode
	v.ideVersionMu.Unlock()
}

// ---- 模型 ----

func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	// 借一个可用凭证探测上游目录（Catalog.StaticModels 只在有凭证时调用本方法）。
	cred, okCred := core.ProbeNativeCredential(v.Env, v.ID(), func(c *core.Credential) (*Credential, bool) {
		native, okNative := c.Native.(*Credential)
		return native, okNative
	})
	if !okCred || cred == nil {
		return nil, fmt.Errorf("trae: 无可用凭证，无法拉取模型目录")
	}
	models, errModels := v.catalog.fetchModels(ctx, cred, v.profile())
	if errModels != nil {
		return nil, errModels
	}
	return v.modelsToPluginAPI(models), nil
}

// modelsToPluginAPI 把上游模型条目转成宿主契约类型，顺带记下倍率表。
//
// 倍率与清单必须出自同一轮探测：分开取会出现「模型换了一批、倍率还是
// 上一批」的错配。
func (v *adapter) modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	core.FillCredits(&v.credits, models,
		func(model Model) string { return model.ID },
		func(model Model) string { return model.Credits })
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
			CanDisableThinking: !model.IsReasoning,
		}, true
	})
}

// ModelCredits 实现 core.CreditsProvider：返回「官方模型 ID → 计费倍率」。
func (v *adapter) ModelCredits() map[string]string {
	return v.credits.Snapshot()
}

// ---- 执行 ----

func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	payload, errChat := Chat(ctx, "", ChatRequest{
		Credential: native,
		Profile:    v.profile(),
		Body:       req.Payload,
		Stream:     false,
		catalog:    &v.catalog,
	})
	if errChat != nil {
		return nil, core.ToPluginError(errChat)
	}
	return &pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	return ChatStream(ctx, "", ChatRequest{
		Credential: native,
		Profile:    v.profile(),
		Body:       req.Payload,
		Stream:     true,
		catalog:    &v.catalog,
	}, sink.Emit)
}

// ---- 额度与签到 ----

func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	response, errQuota := FetchQuota(ctx, native, v.profile())
	if errQuota != nil {
		return nil, core.ToPluginError(errQuota)
	}
	return response, nil
}

func (v *adapter) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	result, errCheckin := Checkin(ctx, native, v.profile())
	if errCheckin != nil {
		return nil, core.ToPluginError(errCheckin)
	}
	return &core.CheckinResult{
		Already: result.Already,
		Credit:  result.Credit,
		Message: result.Message,
	}, nil
}

// ---- 登录与续期 ----

func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return LoginStart(ctx, v.profile(), v.sessions)
}

func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return LoginPoll(ctx, state, v.sessions)
}

// OwnsLoginSession 报告该登录会话是否由本实例持有。
//
// Trae 的会话表自持（回调监听要求实例隔离），没有登记到公共 LoginSessions，
// 根层路由登录请求时靠这个自证接口把 state 认回来。
func (v *adapter) OwnsLoginSession(sessionID string) bool {
	return v.sessions.owns(sessionID, v.profile().VendorID)
}

// LocalCallbackHint 返回控制台要展示的本机回调地址。
func (v *adapter) LocalCallbackHint() string { return v.sessions.callbackHint() }

// SubmitCallback 解析用户粘贴的 localhost 回调链接并完成登录。
func (v *adapter) SubmitCallback(ctx context.Context, state, rawURL string) (*core.CallbackSubmitResult, error) {
	return v.sessions.SubmitCallback(ctx, state, rawURL)
}

func (v *adapter) SupportsCheckin() bool { return SupportsCheckin(v.profile()) }

// ---- 任务族 ----

// TaskKindUG 是「活动福利领取」任务族。
//
// 族名不带供应商前缀：相邻 vendor 的族名都是 dailybonus / travel 这种，宿主
// 的排程键也按族名保存，两套命名口径会让配置看起来像两种东西。
const TaskKindUG = "ug"

// TaskSpecs 声明 Trae 家族自己的任务族。
//
// 只声明活动福利这一个族，且刻意不给 DefaultHours：2 个入口都是活动期才开的
// 领奖口，不是每天都有东西可领，默认排程只会每天白打 2 次空请求。需要时在
// 控制台手动触发（或自己加排程）。国际版没有这套活动，故不声明。
func (v *adapter) TaskSpecs() []core.TaskSpec {
	if !v.SupportsCheckin() {
		return nil
	}
	return []core.TaskSpec{{
		Kind:        TaskKindUG,
		Label:       "活动福利领取",
		Description: "领取 Trae 国内版活动福利：活动中心（竞赛、作品、新用户等）与分享裂变积分",
		// Route 让控制台按 ManualSpecs 自动出现手动触发入口：宿主只注册带
		// Route 的手动族（admin 里 Route 为空直接跳过），少了它这个「手动族」
		// 就只能靠手改排程配置触发。
		Route:  "/ug/claim",
		Manual: true,
	}}
}

// RunScheduled 执行本供应商声明的任务族。
func (v *adapter) RunScheduled(ctx context.Context, cred *core.Credential, kind string) (string, error) {
	if kind != TaskKindUG {
		// 宿主的排程是全局的，别家的族走到这里说明宿主没过滤干净；按「无实现」返回。
		return "", fmt.Errorf("trae: 不支持的任务族 %q", kind)
	}
	native, errNative := v.Native(cred)
	if errNative != nil {
		return "", errNative
	}
	results, errClaim := ClaimUG(ctx, native, v.profile())
	if errClaim != nil {
		if len(results) > 0 {
			// 部分成功：把已拿到的结果一并带出，别让一次取消/凭证问题抹掉战果。
			return SummarizeUG(results), core.ToPluginError(errClaim)
		}
		return "", core.ToPluginError(errClaim)
	}
	return SummarizeUG(results), nil
}

// ---- 一键任务 ----

// AutoTaskCodeUG 是一键任务里「活动福利」那一行的任务码。
const AutoTaskCodeUG = "(活动福利)"

// ugTaskTitle 是聚合活动任务在扫描结果里显示的名字。
const ugTaskTitle = "活动福利领取"

// RunTask 执行单条任务。
//
// 认三种任务码：签到、聚合活动领取、以及动作表里的单个端点 key。其余一律
// 按「该供应商没有这条任务」返回 skipped，避免静默成功。
func (v *adapter) RunTask(ctx context.Context, cred *core.Credential, code string) (core.TaskRunResult, error) {
	trimmed := strings.TrimSpace(code)
	if trimmed == core.AutoTaskCodeCheckin {
		row := core.BuildCheckinAutoTask(ctx, v, cred)
		if row == nil {
			return core.TaskRunResult{Status: core.AutoStatusSkipped, Message: "该账号不支持签到"}, nil
		}
		return core.TaskRunResult{Status: row.Status, Message: row.Message, Credit: row.Credit, Energy: row.Energy}, nil
	}
	if trimmed == AutoTaskCodeUG {
		return v.runUGTask(ctx, cred)
	}
	// 单个端点也能按 key 点名执行：固定端点没有读取接口，扫描产不出具体条目，
	// 只能由控制台/用户按 key 触发。
	if endpoint, okEndpoint := ugEndpointByKey(trimmed); okEndpoint {
		return v.runUGEndpoint(ctx, cred, endpoint)
	}
	return core.TaskRunResult{Status: core.AutoStatusSkipped, Message: "该供应商没有这条任务"}, nil
}

// RunAllTasks 执行一键任务：先签到，再领活动中心/裂变/比赛的活动奖励。
//
// 活动福利全部挂在 UG 端点上，与定时排程的 ug 族是同一套调用；这里只是把它
// 接进一键任务，让用户不用等排程也能立刻把能领的领掉。领取本身幂等。
func (v *adapter) RunAllTasks(ctx context.Context, cred *core.Credential) []core.AutoTaskResult {
	if !v.SupportsCheckin() {
		return nil
	}
	results := make([]core.AutoTaskResult, 0, 2)
	if row := core.BuildCheckinAutoTask(ctx, v, cred); row != nil {
		results = append(results, *row)
	}
	native, errNative := v.Native(cred)
	if errNative != nil {
		return append(results, core.AutoTaskResult{
			Code:    AutoTaskCodeUG,
			Status:  core.AutoStatusError,
			Message: core.ToPluginError(errNative).Error(),
		})
	}
	ugResults, errClaim := ClaimUG(ctx, native, v.profile())
	status, message := ugAutoStatus(ugResults)
	if errClaim != nil {
		if len(ugResults) == 0 {
			return append(results, core.AutoTaskResult{
				Code:    AutoTaskCodeUG,
				Status:  core.AutoStatusError,
				Message: core.ToPluginError(errClaim).Error(),
			})
		}
		// 部分成功：已拿到的战果照常展示，只把错误附在后面。
		message += "；" + core.ToPluginError(errClaim).Error()
	}
	return append(results, core.AutoTaskResult{Code: AutoTaskCodeUG, Status: status, Message: message})
}

// runUGTask 领一轮活动福利，返回三态结果。
func (v *adapter) runUGTask(ctx context.Context, cred *core.Credential) (core.TaskRunResult, error) {
	if !v.SupportsCheckin() {
		return core.TaskRunResult{Status: core.AutoStatusSkipped, Message: "该区域没有活动福利"}, nil
	}
	native, errNative := v.Native(cred)
	if errNative != nil {
		return core.TaskRunResult{}, errNative
	}
	results, errClaim := ClaimUG(ctx, native, v.profile())
	status, message := ugAutoStatus(results)
	if errClaim != nil {
		if len(results) == 0 {
			return core.TaskRunResult{Status: core.AutoStatusError, Message: core.ToPluginError(errClaim).Error()}, nil
		}
		message += "；" + core.ToPluginError(errClaim).Error()
	}
	return core.TaskRunResult{Status: status, Message: message}, nil
}

// runUGEndpoint 执行动作表里的单个端点。
func (v *adapter) runUGEndpoint(ctx context.Context, cred *core.Credential, endpoint UGEndpoint) (core.TaskRunResult, error) {
	if !v.SupportsCheckin() {
		return core.TaskRunResult{Status: core.AutoStatusSkipped, Message: "该区域没有活动福利"}, nil
	}
	native, errNative := v.Native(cred)
	if errNative != nil {
		return core.TaskRunResult{}, errNative
	}
	headers, errHeaders := checkinHeaders(native)
	if errHeaders != nil {
		return core.TaskRunResult{Status: core.AutoStatusError, Message: errHeaders.Error()}, nil
	}
	result, errRun := endpoint.Run(ctx, v.profile(), headers, endpoint)
	if errRun != nil {
		return core.TaskRunResult{Status: core.AutoStatusError, Message: core.ToPluginError(errRun).Error()}, nil
	}
	return core.TaskRunResult{Status: ugResultAutoStatus(result), Message: result.Describe()}, nil
}

// ugAutoStatus 把一轮 UG 结果压成三态 + 一行文案。
//
// 领到东西才算 done；只有失败才算 error；已领过、暂不可领、待满足前置都算
// skipped——把它们记成 error 会让正常账号在日志里天天刷红。
func ugAutoStatus(results []UGResult) (string, string) {
	if len(results) == 0 {
		return core.AutoStatusSkipped, "没有可领取的活动入口"
	}
	var claimed, failed, already int
	for _, result := range results {
		switch result.Status {
		case UGClaimed:
			claimed++
		case UGAlready:
			already++
		case UGFailed:
			failed++
		}
	}
	message := SummarizeUG(results)
	switch {
	case claimed > 0:
		return core.AutoStatusDone, message
	case failed > 0 && already == 0:
		return core.AutoStatusError, message
	default:
		return core.AutoStatusSkipped, message
	}
}

// ugResultAutoStatus 把单条结果映射成三态。
func ugResultAutoStatus(result UGResult) string {
	switch result.Status {
	case UGClaimed:
		return core.AutoStatusDone
	case UGFailed:
		return core.AutoStatusError
	default:
		return core.AutoStatusSkipped
	}
}

// ---- 任务扫描与活动状态 ----

// InspectTasks 扫描账号当前可领的活动福利。
//
// 只报「上游明确说可领且还没发」的活动：签到走专用接口（见 TaskStatus 的
// 状态行），需要用户先在自己客户端里做前置动作的活动插件替不了，报出来只是
// 一行永远不变的提示。多条可领活动聚合成一次调用（RunCode），队列只入队一次。
func (v *adapter) InspectTasks(ctx context.Context, cred *core.Credential) (core.TaskInspection, error) {
	if !v.SupportsCheckin() {
		return core.TaskInspection{}, nil
	}
	native, errNative := v.Native(cred)
	if errNative != nil {
		return core.TaskInspection{}, errNative
	}
	headers, errHeaders := checkinHeaders(native)
	if errHeaders != nil {
		return core.TaskInspection{}, errHeaders
	}
	activities, errActivities := fetchUGActivities(ctx, v.profile(), headers)
	if errActivities != nil {
		return core.TaskInspection{}, core.ToPluginError(errActivities)
	}
	now := time.Now()
	inspection := core.TaskInspection{}
	for _, activity := range activities {
		if !activity.enabled() || activity.expired(now) {
			continue
		}
		inspection.InPeriod = true
		if !claimableActivity(activity, now) {
			continue
		}
		inspection.Items = append(inspection.Items, core.TaskItem{
			Kind:    TaskKindUG,
			Code:    "activity:" + activity.ID,
			RunCode: AutoTaskCodeUG,
			Title:   activity.activityTitle(),
			Target:  1,
		})
	}
	if len(inspection.Items) == 0 {
		// 竞赛/抽奖这类固定端点没有读取接口，扫不出状态。账号支持活动就给一次
		// 机会，否则「活动表里没有可领项」会被误读成「今天无事可做」。
		inspection.Items = append(inspection.Items, core.TaskItem{
			Kind: TaskKindUG, Code: AutoTaskCodeUG, Title: ugTaskTitle, Target: 1,
		})
	}
	return inspection, nil
}

// TaskStatus 返回活动任务状态（供控制台展示）。
//
// 行 = 每日签到 + 活动表里每一条未结束的活动。状态用短 token 而不是中文：
// 控制台按它上色/过滤，改文案不该影响判断逻辑。
func (v *adapter) TaskStatus(ctx context.Context, cred *core.Credential) core.TaskStatusResult {
	if !v.SupportsCheckin() {
		return core.TaskStatusResult{Error: "国际版没有活动福利"}
	}
	native, errNative := v.Native(cred)
	if errNative != nil {
		return core.TaskStatusResult{Error: errNative.Error()}
	}
	headers, errHeaders := checkinHeaders(native)
	if errHeaders != nil {
		return core.TaskStatusResult{Error: errHeaders.Error()}
	}
	result := core.TaskStatusResult{}
	// 签到状态预查失败不阻断活动表：两段各报各的，别让一个端点故障抹掉全表。
	if status, errStatus := fetchCheckinStatus(ctx, v.profile(), headers); errStatus == nil && status != nil {
		view := core.TaskStatusView{Code: core.AutoTaskCodeCheckin, Title: "每日签到", Target: 1}
		switch {
		case !status.Enable:
			view.Status = "disabled"
		case status.CheckedIn:
			view.Status = "checked_in"
			view.Progress = 1
		default:
			view.Status = "pending"
		}
		result.Tasks = append(result.Tasks, view)
		result.InPeriod = status.Enable
	}
	activities, errActivities := fetchUGActivities(ctx, v.profile(), headers)
	if errActivities != nil {
		result.Error = core.ToPluginError(errActivities).Error()
		return result
	}
	now := time.Now()
	for _, activity := range activities {
		if !activity.enabled() || activity.expired(now) {
			continue
		}
		result.InPeriod = true
		result.Tasks = append(result.Tasks, core.TaskStatusView{
			Code:   "activity:" + activity.ID,
			Title:  activity.activityTitle(),
			Status: activityStatusToken(activity, now),
			Target: 1,
		})
	}
	return result
}

// 编译期断言：适配器实际具备的供应商能力。
var (
	_ core.Vendor              = (*adapter)(nil)
	_ core.CallbackSubmitter   = (*adapter)(nil)
	_ core.CheckinProvider     = (*adapter)(nil)
	_ core.Configurable        = (*adapter)(nil)
	_ core.LoginProvider       = (*adapter)(nil)
	_ core.LoginSessionOwner   = (*adapter)(nil)
	_ core.QuotaProvider       = (*adapter)(nil)
	_ core.CreditsProvider     = (*adapter)(nil)
	_ core.TaskCatalogProvider = (*adapter)(nil)
	_ core.ScheduledRunner     = (*adapter)(nil)
	_ core.TaskRunner          = (*adapter)(nil)
	_ core.TaskInspector       = (*adapter)(nil)
	_ core.TaskStatusProvider  = (*adapter)(nil)
)
