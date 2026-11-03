package core

import (
	"context"
	"strings"
)

type TaskSpec struct {
	Kind         string `json:"kind"`
	Label        string `json:"label"`
	Description  string `json:"description,omitempty"`
	DefaultHours []int  `json:"default_hours,omitempty"`
	AfterCheckin bool   `json:"after_checkin,omitempty"`
	Manual       bool   `json:"manual,omitempty"`
	UsesChat     bool   `json:"uses_chat,omitempty"`
	Route        string `json:"route,omitempty"`
}

// TaskCatalogProvider 由需要注册任务族/排程的供应商实现。
type TaskCatalogProvider interface {
	TaskSpecs() []TaskSpec
}

// SupportsScheduledKind 报告供应商是否声明了某个定时任务族。
//
// 排程（state.json 的 task_schedules）是全局的，不区分供应商与账号；调度器在
// 执行前必须按各家声明的 TaskSpecs 过滤：未声明的族对它没有实现，跳过即可，
// 不能落 ERROR——那会把「这个功能不属于你」误报成故障（issue #58）。
func SupportsScheduledKind(vendor Vendor, kind string) bool {
	catalog, okCatalog := As[TaskCatalogProvider](vendor)
	if !okCatalog {
		return false
	}
	kind = strings.TrimSpace(kind)
	for _, spec := range catalog.TaskSpecs() {
		if spec.Kind == kind {
			return true
		}
	}
	return false
}

// 任务调度的中立类型与接口。
//
// 供应商只在支持任务/签到时实现这些接口；根层调度器按类型断言调用，
// 不支持的供应商直接跳过，不需要任何占位实现。

// 通用任务族标识。供应商专属任务族由各供应商的 TaskSpec 提供。
const (
	TaskKindCheckin   = "checkin"
	TaskKindKeepalive = "keepalive"
)

// TaskItem 是一条待办任务（扫描结果）。
type TaskItem struct {
	Kind string `json:"kind"`
	Code string `json:"code"`
	// RunCode 非空时表示这是一条聚合任务：队列只入队一次，并以此任务码执行。
	// 用于把多个同类待办（例如开学季活动）合并为一次供应商调用。
	RunCode  string `json:"run_code,omitempty"`
	Title    string `json:"title"`
	Current  int64  `json:"current"`
	Target   int64  `json:"target"`
	UsesChat bool   `json:"uses_chat"`
}

// TaskInspection 是一次账号扫描的结果。
type TaskInspection struct {
	Items    []TaskItem
	InPeriod bool
}

// TaskRunResult 是单条任务的执行结果。
type TaskRunResult struct {
	Status  string
	Message string
	Credit  int64
	Energy  int64
}

// AutoTaskResult 是一键完成里单条任务的结果（含任务码与最终进度）。
type AutoTaskResult struct {
	Code     string
	Status   string
	Message  string
	Credit   int64
	Energy   int64
	Progress string
}

// TaskStatusView 是任务状态的展示形态。
type TaskStatusView struct {
	Code     string
	Title    string
	Status   string
	Progress int64
	Target   int64
}

// TaskStatusResult 是任务活动状态。
type TaskStatusResult struct {
	InPeriod bool
	Tasks    []TaskStatusView
	Chances  any
	Error    string
}

// TaskRunner 由支持自动任务执行的供应商实现。
type TaskRunner interface {
	// RunTask 执行单条任务。
	RunTask(ctx context.Context, cred *Credential, code string) (TaskRunResult, error)
	// RunAllTasks 按依赖序执行全部可自动完成的任务。
	RunAllTasks(ctx context.Context, cred *Credential) []AutoTaskResult
}

// TaskInspector 由支持任务扫描的供应商实现。
type TaskInspector interface {
	InspectTasks(ctx context.Context, cred *Credential) (TaskInspection, error)
}

// TaskStatusProvider 由有活动任务状态的供应商实现。
type TaskStatusProvider interface {
	TaskStatus(ctx context.Context, cred *Credential) TaskStatusResult
}

// ScheduledRunner 由需要执行定时任务族的供应商实现。
type ScheduledRunner interface {
	RunScheduled(ctx context.Context, cred *Credential, kind string) (message string, err error)
}

// TaskPreparer 由需要在执行前批量报名/准备的供应商实现。
type TaskPreparer interface {
	PrepareTasks(ctx context.Context, cred *Credential) int
}

// 一键任务的结果三态。
//
// 必须分开：把「无事可做」算成 done 会让用户以为今天领到了东西；把「跳过」
// 算成 error 又会让正常账号在日志里刷红。
const (
	AutoStatusDone    = "done"
	AutoStatusSkipped = "skipped"
	AutoStatusError   = "error"
)

// AutoTaskCodeCheckin 是一键任务里代表「签到」那一行的任务码。
//
// 各家的签到本身就是最主要的每日福利（autoclaw 的登录活跃、minimaxcode 的
// 每日礼包、raccoon 的桌面端登录积分都在这里），所以把它当成一条普通任务
// 执行，而不是另开一个按钮。
const AutoTaskCodeCheckin = "(签到)"

// BuildCheckinAutoTask 把供应商的签到折叠成一键任务里的一行。
//
// 返回 nil 表示该账号不该出现这一行：供应商没有签到能力，或该账号天生不支持
// 签到。调用方直接跳过，不要补一条 skipped——那会让用户以为「本该有却失败了」。
func BuildCheckinAutoTask(ctx context.Context, vendor Vendor, cred *Credential) *AutoTaskResult {
	provider, okProvider := As[CheckinProvider](vendor)
	if !okProvider || provider == nil || !provider.SupportsCheckin() {
		return nil
	}
	if capable, okCapable := As[CheckinCapability](vendor); okCapable && !capable.SupportsCheckinFor(cred) {
		return nil
	}
	result, errCheckin := provider.Checkin(ctx, cred)
	if errCheckin != nil {
		return &AutoTaskResult{Code: AutoTaskCodeCheckin, Status: AutoStatusError, Message: errCheckin.Error()}
	}
	if result == nil {
		return nil
	}
	status := AutoStatusDone
	message := result.Message
	switch {
	case result.NotRecorded:
		// 「窗口未开启 / 领取被拒 / 上游没发积分」都是本次没真正领到：记成
		// done 会让用户以为今天领到了东西，必须如实算作跳过。
		status = AutoStatusSkipped
		if message == "" {
			message = "本次未记录（上游窗口未开启或领取被拒）"
		}
	case result.Already:
		status = AutoStatusSkipped
		if message == "" {
			message = "今日已签到"
		}
	case message == "":
		message = "签到成功"
	}
	return &AutoTaskResult{
		Code:    AutoTaskCodeCheckin,
		Status:  status,
		Message: message,
		Credit:  result.Credit,
		Energy:  result.Energy,
	}
}
