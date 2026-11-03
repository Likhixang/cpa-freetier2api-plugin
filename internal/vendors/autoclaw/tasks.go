package autoclaw

// 本文件实现 AutoClaw 的账号运营能力（当前为每日签到；POST {base}/autoclaw-proxy/proxy/autoclaw-task-complete）。
//
// 协议依据 Agent2API check.py：body {"task_id":"daily_signin"}，业务信封
// code=0 成功；data.already_completed=true 表示今日已签（幂等），
// data.reward_points 是本次奖励积分。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// checkinTaskID 是每日签到任务的任务 ID。
const checkinTaskID = "daily_signin"

// CheckinResult 是一次签到的结果。
type CheckinResult struct {
	Already bool
	// Claimed 报告本次是否真的领到了积分。上游可能以 code=0 返回
	// success:false（窗口未开 / 无资格），此时不能算签到成功。
	Claimed bool
	Points  int64
	TaskID  string
	// Msg 是上游给出的可读说明（失败时用于展示）。
	Msg string
}

// Checkin 执行一次每日签到。
func Checkin(ctx context.Context, baseURL string, cred *Credential) (*CheckinResult, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("upstream base URL is not configured")
	}
	body, errMarshal := json.Marshal(map[string]any{"task_id": checkinTaskID})
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal checkin request: %w", errMarshal)
	}
	endpoint := baseURL + CheckinPath
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if errReq != nil {
		return nil, fmt.Errorf("build checkin request: %w", errReq)
	}
	ApplyUserAPIHeaders(req, cred.AccessTokenValue())

	resp, errDo := hostbridge.Client(ctx, 20*time.Second).Do(req)
	if errDo != nil {
		return nil, &core.Failure{Msg: "checkin: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read checkin response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	var envelope struct {
		Code *int64 `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Success          bool   `json:"success"`
			AlreadyCompleted bool   `json:"already_completed"`
			RewardPoints     any    `json:"reward_points"`
			TaskID           string `json:"task_id"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return nil, Classify(resp.StatusCode, 0, string(raw))
	}
	points := int64(core.IntFromAny(envelope.Data.RewardPoints))

	// 业务码优先：登录失效按 401 上抛，其余非零码是「本次没领成」而不是
	// 网络故障——当成 transient 会无限重试。
	if envelope.Code != nil && *envelope.Code != 0 {
		if DeadCode(*envelope.Code) {
			return nil, Classify(http.StatusUnauthorized, *envelope.Code, envelope.Msg)
		}
		if resp.StatusCode >= 400 {
			return nil, Classify(resp.StatusCode, *envelope.Code, envelope.Msg)
		}
		// HTTP 200 里带业务码：会话失效的文案要按凭证错误上抛（宿主才会禁用
		// 这个号），其它码是「本次没领成」而不是网络故障——当成 transient 会
		// 无限重试。
		if core.ContainsAny(strings.ToLower(envelope.Msg), deadMarkers) {
			return nil, Classify(http.StatusUnauthorized, *envelope.Code, envelope.Msg)
		}
		return &CheckinResult{
			Already: envelope.Data.AlreadyCompleted,
			Msg:     core.FirstNonEmpty(envelope.Msg, "签到未完成"),
		}, nil
	}

	// code=0 / 缺省：以 data.success + reward_points 判定是否真的领到。
	// 参考实现：claimed = reward_points > 0 || (success && !already_completed)。
	claimed := points > 0 || (envelope.Data.Success && !envelope.Data.AlreadyCompleted)
	if resp.StatusCode >= 400 && !claimed && !envelope.Data.AlreadyCompleted {
		return nil, Classify(resp.StatusCode, 0, string(raw))
	}
	return &CheckinResult{
		Already: envelope.Data.AlreadyCompleted,
		Claimed: claimed,
		Points:  points,
		TaskID:  checkinTaskID,
		Msg:     envelope.Msg,
	}, nil
}

// UserInfoPath 是登录活跃上报端点（签到成功后由服务端发放活动奖励）。
const UserInfoPath = "/userapi/v1/user-info"

// TriggerLoginActive 在签到成功后上报一次登录活跃。
//
// 参考实现（Agent2API checkin.rs）在签到后 POST user-info，服务端据此发放
// 每日活跃 / 一次性登录奖励。失败只记日志：它是签到之外的附加收益，既不能
// 改变签到结论，也不值得重试。
func TriggerLoginActive(ctx context.Context, baseURL string, cred *Credential) {
	if errActive := reportLoginActive(ctx, baseURL, cred); errActive != nil {
		logger.Debug("autoclaw: login-active report failed: %v", errActive)
	}
}

func reportLoginActive(ctx context.Context, baseURL string, cred *Credential) error {
	deviceID := strings.TrimSpace(cred.DeviceIDValue())
	if strings.TrimSpace(baseURL) == "" || deviceID == "" {
		return nil
	}
	body, errMarshal := json.Marshal(map[string]string{"source_id": "autoclaw", "device_id": deviceID})
	if errMarshal != nil {
		return errMarshal
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+UserInfoPath, bytes.NewReader(body))
	if errReq != nil {
		return errReq
	}
	ApplyUserAPIHeaders(req, cred.AccessTokenValue())
	resp, errDo := hostbridge.Client(ctx, 20*time.Second).Do(req)
	if errDo != nil {
		// 传输层失败归 transient：宿主换号重试是安全的。
		return &core.Failure{Kind: core.FailureKindTransient, Msg: "login-active: " + errDo.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	// 读回执：失败时上游把原因写在正文里（会话失效、账号被限），丢掉就只能
	// 报一个裸状态码。回执内容对成功路径无关紧要。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if resp.StatusCode >= 400 {
		// 走统一分类：401/403 归会话失效、402 归余额、429 归限流，
		// 而不是一律 transient 让宿主反复换号重试。
		return Classify(resp.StatusCode, 0, string(raw))
	}
	return nil
}

// ---- 一键任务 ----
//
// 上游能自动完成的运营动作只有两条：签到（autoclaw-task-complete）与登录
// 活跃上报（user-info）。活动奖励由服务端按这两条事件发放，没有别的领取
// 入口，因此一键任务就按下面这张表逐行执行，表外的动作一律不做。

// AutoTaskCodeLoginActive 是一键任务里「登录活跃」那一行的任务码。
const AutoTaskCodeLoginActive = "(登录活跃)"

// autoTask 是一键任务表的一行。
type autoTask struct {
	code  string
	label string
	// skip 返回非空原因时本行不执行（→ skipped，原因即此）。
	skip func(*autoRun) string
	// exec 执行本行；返回空消息表示无事可做（→ skipped）。
	exec func(context.Context, *autoRun) (string, int64, error)
}

// autoRun 是一轮一键任务的共享状态。
type autoRun struct {
	baseURL string
	cred    *Credential
	// loginActiveReported 报告签到流程是否已经代劳了登录活跃上报。
	// 重复上报没有额外收益，能省一次就省一次。
	loginActiveReported bool
}

// autoTasks 是一键任务表（顺序即依赖序）。
var autoTasks = []autoTask{
	{
		code:  AutoTaskCodeLoginActive,
		label: "登录活跃",
		skip: func(run *autoRun) string {
			if strings.TrimSpace(run.cred.DeviceIDValue()) == "" {
				return "凭证缺少 device_id，无法上报登录活跃"
			}
			if run.loginActiveReported {
				return "签到流程已上报登录活跃，本次不重复上报"
			}
			return ""
		},
		exec: func(ctx context.Context, run *autoRun) (string, int64, error) {
			if errActive := reportLoginActive(ctx, run.baseURL, run.cred); errActive != nil {
				return "", 0, errActive
			}
			return "已上报登录活跃（服务端据此发放每日活跃奖励）", 0, nil
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

// run 执行一行任务：前置不成立即 skipped，执行失败即 error，空消息即 skipped。
func (t autoTask) run(ctx context.Context, run *autoRun) core.AutoTaskResult {
	result := core.AutoTaskResult{Code: t.code}
	if t.skip != nil {
		if reason := t.skip(run); reason != "" {
			result.Status = core.AutoStatusSkipped
			result.Message = reason
			return result
		}
	}
	message, credit, errExec := t.exec(ctx, run)
	result.Credit = credit
	switch {
	case errExec != nil:
		result.Status = core.AutoStatusError
		result.Message = t.label + "：" + reasonOf(errExec)
	case strings.TrimSpace(message) == "":
		result.Status = core.AutoStatusSkipped
		result.Message = "当前无需动作"
	default:
		result.Status = core.AutoStatusDone
		result.Message = message
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

// reasonOf 取用户可读的失败原因：分类错误用上游原文，避免把
// "upstream soft_rate [429]:" 这类内部分类前缀怼到页面上。
func reasonOf(err error) string {
	var failure *core.Failure
	if errors.As(err, &failure) && strings.TrimSpace(failure.Msg) != "" {
		return failure.Msg
	}
	return err.Error()
}
