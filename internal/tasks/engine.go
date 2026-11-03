package tasks

// 本文件是通用任务编排：
//   - 调度器：整点小时列表唤醒，按任务族并行派发；
//   - 任务中心：账号扫描、执行队列、单任务/一键完成、活动任务状态汇总。
//
// 具体任务动作与上游协议在供应商包（如 workbuddy），这里只认 core 能力接口。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/internal/auth"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/state"
)

// 调度循环参数。
const (
	// schedulerWakeGrace 是迟到唤醒的宽限等待（休眠恢复后网络栈需要时间）。
	schedulerWakeGrace = 5 * time.Second
	// schedulerLateThreshold 是判定「迟到」的阈值。
	schedulerLateThreshold = 1 * time.Second
	// schedulerReconfigure 是配置变更信号缓冲。
	schedulerReconfigure = "reconfigure"
	// accountGap 是账号之间的操作间隔。
	accountGap = 800 * time.Millisecond
)

// runTaskScheduler 是后台调度循环。
func (s *Service) runTaskScheduler(ctx context.Context) {
	logger.Info("task scheduler started")
	s.schedulerMu.Lock()
	s.running = true
	s.schedulerMu.Unlock()
	defer func() {
		s.schedulerMu.Lock()
		s.running = false
		s.schedulerMu.Unlock()
		logger.Info("task scheduler stopped")
	}()
	for {
		kinds, next := s.nextScheduledWake(s.deps.Now())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case signal := <-s.pokeCh:
			timer.Stop()
			if signal == schedulerReconfigure {
				logger.Debug("task scheduler reconfigured")
			}
			continue
		case <-timer.C:
			if delay := time.Since(next); delay > schedulerLateThreshold {
				logger.Debug("task scheduler woke %s late, waiting grace period", delay.Round(time.Millisecond))
				if !core.SleepContext(ctx, schedulerWakeGrace) {
					return
				}
			}
			s.runTaskBatch(ctx, kinds)
		}
	}
}

// runTaskBatch 并行派发同一时刻的多个任务族。
func (s *Service) runTaskBatch(ctx context.Context, kinds []string) {
	var waitGroup sync.WaitGroup
	for _, kind := range kinds {
		waitGroup.Add(1)
		go func(name string) {
			defer waitGroup.Done()
			s.runScheduledTaskContext(ctx, name)
		}(kind)
	}
	waitGroup.Wait()
}

// nextScheduledWake 返回下一次唤醒时刻与要执行的任务族。
func (s *Service) nextScheduledWake(now time.Time) ([]string, time.Time) {
	return NextWake(now, s.effectiveSchedule())
}

// effectiveSchedule 返回生效的排程（页面设置优先于默认值）。
func (s *Service) effectiveSchedule() map[string][]int {
	schedule := s.catalog.EffectiveSchedule(s.deps.State().TaskSchedules)
	// auto_checkin_at 是用户明确配置的签到时刻，始终并入排程。
	//
	// 原先只在「任务排程里没有 checkin 覆盖」时才生效，但 task_schedules 在
	// 首次启用任务时就会被写入默认值 [9,21]，于是页面上设置的 10:00 永远
	// 被忽略——表现为「设了自动签到时间不生效」，Qoder 的 10:00 领取窗口
	// 也永远跑空。这里改成并集：排程里的点保留作重试，设置里的点必定执行。
	if s.effectiveAutoCheckin() {
		if hour, okHour := parseClockHour(s.checkinAt()); okHour {
			schedule[core.TaskKindCheckin] = mergeHour(schedule[core.TaskKindCheckin], hour)
		}
	}
	return schedule
}

// checkinAt 返回生效的自动签到时刻（页面设置优先）。
func (s *Service) checkinAt() string {
	if at := strings.TrimSpace(s.deps.State().AutoCheckinAt); at != "" {
		return at
	}
	return strings.TrimSpace(s.deps.Config().AutoCheckinAt)
}

// mergeHour 把一个整点并进排程小时列表，复用 NormalizeHours 做去重与排序。
//
// 这里曾自己写一份去重+排序，和 NormalizeHours 的规则分头演进（前者不校验
// 0-23 边界、后者校验），并出来一条 24 点的排程不会被任何人发现。
func mergeHour(hours []int, hour int) []int {
	return NormalizeHours(append(hours, hour))
}

// parseClockHour 把 HH:MM 转成排程用的整点小时。
//
// 排程粒度是整点，分钟部分向上取整而不是丢弃：用户写 10:35 就是希望
// 10:35 之后才跑（签到窗口常常 10:30 才开），按 10:00 执行会跑空。
// 23:xx 取整后落到次日 00:00，同样不早于设定时刻。
func parseClockHour(value string) (int, bool) {
	hour, minute, ok := core.ParseClock(value)
	if !ok {
		return 0, false
	}
	if minute > 0 {
		hour = (hour + 1) % 24
	}
	return hour, true
}

// pokeScheduler 唤醒调度循环重新计算下次执行时刻。

// runScheduledTask 在后台执行一个任务族（供管理接口手动触发）。

// runScheduledTaskContext 执行一个任务族。
func (s *Service) runScheduledTaskContext(ctx context.Context, kind string) {
	if kind == core.TaskKindCheckin {
		s.runCheckinAll(ctx)
		return
	}
	s.runScheduledFamily(ctx, kind)
}

// schedulerStatus 返回调度器状态（供管理页展示）。
func (s *Service) schedulerStatus() map[string]any {
	s.schedulerMu.Lock()
	running := s.running
	s.schedulerMu.Unlock()

	schedule := s.effectiveSchedule()
	kinds, next := s.nextScheduledWake(s.deps.Now())
	readable := make(map[string][]int, len(schedule))
	for kind, hours := range schedule {
		readable[kind] = hours
	}
	// 页面上直接显示「下次执行」的时刻，不要贴 RFC3339：面板用户看到的应该是
	// 「10:00」而不是「2026-10-31T10:00:00+08:00」。跨天时补上日期。
	now := s.deps.Now()
	nextLabel := next.Format("15:04")
	if next.Format("2006-01-02") != now.Format("2006-01-02") {
		nextLabel = next.Format("01-02 15:04")
	}
	return map[string]any{
		"running":    running,
		"schedule":   readable,
		"next_at":    next.Format(time.RFC3339),
		"next_hhmm":  nextLabel,
		"next_tasks": kinds,
		"auto_tasks": s.effectiveAutoTasks(),
		"task_specs": s.catalog.Specs(),
	}
}

// effectiveAutoTasks 返回是否启用任务自动闭环（页面设置优先）。
func (s *Service) effectiveAutoTasks() bool {
	state := s.deps.State()
	if state.AutoTasks != nil {
		return *state.AutoTasks
	}
	return s.deps.Config().AutoTasks
}

// effectiveAutoCheckin 返回是否启用自动签到（页面设置优先）。
func (s *Service) effectiveAutoCheckin() bool {
	state := s.deps.State()
	if state.AutoCheckin != nil {
		return *state.AutoCheckin
	}
	return s.deps.Config().AutoCheckin
}

// effectiveSettings 返回页面设置与配置合并后的可读视图。
func (s *Service) effectiveSettings() map[string]any {
	return map[string]any{
		"auto_checkin":    s.effectiveAutoCheckin(),
		"auto_checkin_at": s.checkinAt(),
		"auto_tasks":      s.effectiveAutoTasks(),
		"schedule":        s.effectiveSchedule(),
		"task_specs":      s.catalog.Specs(),
	}
}

// forEachAccount 遍历本插件名下可用的账号。
//
// targets 为空表示全部账号；非空时只遍历命中的那些（管理端的批量勾选）。
func (s *Service) forEachAccount(ctx context.Context, callbackID string, targets []string, fn func(context.Context, *accountContext) error) {
	cfg := s.deps.Config()
	accounts, failures := s.deps.Pool().Load(ctx, callbackID, auth.Options{
		Targets:       targets,
		EnabledRealms: cfg.EnabledRealms,
	})
	for _, failure := range failures {
		logger.Debug("skip account %s: %s", core.FirstNonEmpty(failure.Entry.Name, failure.Entry.ID), failure.Message)
	}
	for _, account := range accounts {
		if errRun := fn(ctx, newAccountContext(account)); errRun != nil {
			logger.Debug("account %s task failed: %v", core.FirstNonEmpty(account.Entry.Label, account.Entry.Name), errRun)
		}
		if !core.SleepContext(ctx, accountGap) {
			return
		}
	}
}

// accountContext 是遍历中单个账号的上下文。
//
// 除了身份与凭证，还带上原始 JSON 与来源文件路径：凭证续期后要靠这两样
// 才能把新凭证合并回宿主与磁盘（见 keepalive.go）。
type accountContext struct {
	entry      core.HostAuth
	vendor     core.Vendor
	credential *core.Credential
	rawJSON    []byte
	filePath   string
}

// newAccountContext 把账号池条目转成遍历上下文。
func newAccountContext(item auth.Account) *accountContext {
	if item.FilePath != "" {
		item.Credential.SetFilePath(item.FilePath)
	}
	return &accountContext{
		entry: item.Entry, vendor: item.Vendor, credential: item.Credential,
		rawJSON: item.RawJSON, filePath: item.FilePath,
	}
}

func (a *accountContext) uid() string { return a.credential.FileIDValue() }

func (a *accountContext) label() string {
	return core.FirstNonEmpty(a.credential.LabelValue(), a.entry.Label, a.entry.Name, a.entry.ID)
}

// ---- 定时任务族（原 tasks_schedule.go） ----

// runCheckinAll 执行全账号签到，并在启用自动任务时驱动从属闭环。
func (s *Service) runCheckinAll(ctx context.Context) {
	if !s.effectiveAutoCheckin() {
		logger.Debug("checkin skipped: auto checkin is disabled")
		return
	}
	logger.Info("checkin: starting for all accounts")
	count := 0
	s.forEachAccount(ctx, "", nil, func(ctx context.Context, account *accountContext) error {
		if !core.SupportsCheckinFor(account.vendor, account.credential) {
			return nil
		}
		result, errCheckin := s.Checkin(ctx, account.vendor, account.credential, CheckinTriggerSchedule)
		if errCheckin != nil {
			logger.Error("checkin %s: %v", account.label(), errCheckin)
			return nil
		}
		switch {
		case result.NotRecorded:
			// 本次没真正领到（窗口未开启 / 领取被拒 / 未确认发放）。
			// 用 Info 而不是 Error：上游窗口未开启是正常状态，不是故障；
			// 但措辞要如实，别让它看起来像签到成功了。
			logger.Info("checkin %s: not completed: %s", account.label(), result.Message)
		case result.Already:
			logger.Info("checkin %s: already checked in today", account.label())
		default:
			logger.Info("checkin %s: %s", account.label(), result.Message)
		}
		count++
		return nil
	})
	logger.Info("checkin: finished for %d account(s)", count)

	if s.effectiveAutoTasks() {
		for _, spec := range s.catalog.Specs() {
			if spec.AfterCheckin {
				s.runScheduledFamily(ctx, spec.Kind)
			}
		}
	}
}

// runScheduledFamily 遍历账号，把任务族交给供应商的可选实现。
//
// keepalive 是唯一的例外：它不需要供应商写代码，只需实现 core.Refresher
// 刷新 token，写回由引擎统一做（见 keepalive.go）。这样「合并 → 写宿主 →
// 落盘」只有一份实现，供应商包也不必依赖 internal/hostbridge。
func (s *Service) runScheduledFamily(ctx context.Context, kind string) {
	logger.Info("%s: starting for all accounts", kind)
	done, idle, failed := 0, 0, 0
	s.forEachAccount(ctx, "", nil, func(ctx context.Context, account *accountContext) error {
		if kind == core.TaskKindKeepalive {
			result := s.keepaliveOne(ctx, "", account)
			if !result.OK {
				logger.Error("%s %s: %s", kind, account.label(), result.Message)
				failed++
				return nil
			}
			logger.Info("%s %s: %s", kind, account.label(), result.Message)
			done++
			return nil
		}
		message, errRun := s.runScheduledForAccount(ctx, account, kind)
		switch {
		case errRun != nil:
			logger.Error("%s %s: %v", kind, account.label(), errRun)
			failed++
		case message == "":
			idle++
		default:
			logger.Info("%s %s: %s", kind, account.label(), message)
			done++
		}
		return nil
	})
	// 「finished for 0 account(s)」曾经把「没事可做」和「全挂了」显示成同一个
	// 数字：失败分支返回空字符串，计数不增也不报错。三态分开报，别让人对着
	// 一个 0 猜今天到底跑没跑。
	if idle > 0 || failed > 0 {
		logger.Info("%s: finished for %d account(s)（无事可做 %d，失败 %d）", kind, done, idle, failed)
		return
	}
	logger.Info("%s: finished for %d account(s)", kind, done)
}

// runScheduledForAccount 让账号所属供应商执行一个定时任务族。
//
// 排程是全局的（task_schedules 不区分供应商与账号），执行前必须按供应商声明的
// TaskSpecs 过滤：未声明的族对它没有实现，静默跳过即可，不能落 ERROR——否则
// 每到一个排程点就刷一条「unknown scheduled task kind」，把真实错误淹没（issue #58）。
//
// 返回值区分三态：message 非空 = 真做了事；message 空且 err 为 nil = 无事可做；
// err 非空 = 失败。供应商不许把失败折成空字符串，那样上层只能瞎猜。
func (s *Service) runScheduledForAccount(ctx context.Context, account *accountContext, kind string) (string, error) {
	runner, okRunner := core.As[core.ScheduledRunner](account.vendor)
	if !okRunner || !core.SupportsScheduledKind(account.vendor, kind) {
		return "", nil
	}
	return runner.RunScheduled(ctx, account.credential, kind)
}

const (
	// queueMaxConcurrency 是账号间并发的上限。
	queueMaxConcurrency = 4
)

// accountLock 返回某个账号的锁句柄（容量 1 的 channel，放进一个值即持有）。
//
// 用 channel 而不是 sync.Mutex：Mutex.Lock 不可取消，批量任务在 ctx 取消后
// 仍会永久卡在 Lock 上；channel 版本能和 ctx.Done() 一起 select，取消即走。
func (s *Service) accountLock(uid string) chan struct{} {
	trimmed := strings.TrimSpace(uid)
	if trimmed == "" {
		return nil
	}
	s.taskLockMu.Lock()
	defer s.taskLockMu.Unlock()
	if s.taskLocks == nil {
		s.taskLocks = map[string]chan struct{}{}
	}
	lock, okLock := s.taskLocks[trimmed]
	if !okLock {
		lock = make(chan struct{}, 1)
		s.taskLocks[trimmed] = lock
	}
	return lock
}

// tryLockAccount 非阻塞抢锁。手动单任务用这个：抢不到就明确告诉用户
// 「该账号有任务正在执行」，不让人对着转圈等。
func (s *Service) tryLockAccount(uid string) bool {
	lock := s.accountLock(uid)
	if lock == nil {
		return false
	}
	select {
	case lock <- struct{}{}:
		return true
	default:
		return false
	}
}

// lockAccount 排队等锁，ctx 取消时返回错误。
//
// 批量动作（一键完成、任务队列）必须用这个：它们和彼此抢的是同一把账号锁，
// 抢不到就跳过会让用户看到一堆「该账号有其它任务动作在执行」的假失败——实际
// 上只是排在后面。排队才是正确语义。
func (s *Service) lockAccount(ctx context.Context, uid string) error {
	lock := s.accountLock(uid)
	if lock == nil {
		return fmt.Errorf("账号标识为空，无法排队")
	}
	select {
	case lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) unlockAccount(uid string) {
	lock := s.accountLock(uid)
	if lock == nil {
		return
	}
	select {
	case <-lock:
	default:
	}
}

// taskItem 是扫描结果里的一条待办（JSON 契约由 core.TaskItem 定义）。
type taskItem = core.TaskItem

// accountScan 是单账号的扫描结果。
type accountScan struct {
	UID      string     `json:"uid"`
	Label    string     `json:"label"`
	Realm    string     `json:"realm"`
	Tasks    []taskItem `json:"-"`
	InPeriod bool       `json:"in_period"`
	Error    string     `json:"error,omitempty"`
}

// MarshalJSON 保留旧的 growth/school 响应字段，内部统一使用 Tasks。
func (s accountScan) MarshalJSON() ([]byte, error) {
	var growth, school []taskItem
	for _, task := range s.Tasks {
		switch task.Kind {
		case "growth":
			growth = append(growth, task)
		case "school":
			school = append(school, task)
		}
	}
	return json.Marshal(struct {
		UID      string     `json:"uid"`
		Label    string     `json:"label"`
		Realm    string     `json:"realm"`
		Growth   []taskItem `json:"growth"`
		School   []taskItem `json:"school"`
		InPeriod bool       `json:"in_period"`
		Error    string     `json:"error,omitempty"`
	}{s.UID, s.Label, s.Realm, growth, school, s.InPeriod, s.Error})
}

// scanAllAccounts 并发扫描每个账号的待办任务。
func (s *Service) scanAllAccounts(ctx context.Context, callbackID string) []accountScan {
	loaded := s.collectAccounts(ctx, callbackID, nil)
	scans := auth.RunBatch(loaded, queueMaxConcurrency, func(account *accountContext) accountScan {
		return s.scanOneAccount(ctx, account)
	})
	sort.Slice(scans, func(i, j int) bool { return scans[i].Label < scans[j].Label })
	return scans
}

// scanOneAccount 扫描单个账号。
func (s *Service) scanOneAccount(ctx context.Context, account *accountContext) accountScan {
	scan := accountScan{
		UID:   account.uid(),
		Label: account.label(),
		Realm: account.vendor.Region(),
	}
	inspector, okInspector := core.As[core.TaskInspector](account.vendor)
	if !okInspector {
		return scan
	}
	inspection, errInspect := inspector.InspectTasks(ctx, account.credential)
	if errInspect != nil {
		scan.Error = errInspect.Error()
		return scan
	}
	scan.InPeriod = inspection.InPeriod
	scan.Tasks = inspection.Items
	return scan
}

// 执行队列。

type queueItem struct {
	UID      string `json:"uid"`
	Label    string `json:"label"`
	Kind     string `json:"kind"`
	Code     string `json:"code"`
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	Credit   int64  `json:"credit,omitempty"`
	Energy   int64  `json:"energy,omitempty"`
	Progress string `json:"progress,omitempty"`
}

type queueState struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	seq       int64
	conc      int
	items     []queueItem
}

func (s *Service) taskQueueSnapshot() map[string]any {
	s.queue.mu.Lock()
	defer s.queue.mu.Unlock()
	items := make([]queueItem, len(s.queue.items))
	copy(items, s.queue.items)

	done := 0
	for _, item := range items {
		switch item.Status {
		case "done", "error", "skipped":
			done++
		}
	}
	snapshot := map[string]any{
		"running": s.queue.running,
		"seq":     s.queue.seq,
		"conc":    s.queue.conc,
		"total":   len(items),
		"done":    done,
		"items":   items,
	}
	if !s.queue.startedAt.IsZero() {
		snapshot["started_at"] = s.queue.startedAt.Format(time.RFC3339)
	}
	return snapshot
}

// buildQueueItems 把扫描结果摊平成队列项（账号内按依赖序排列）。
//
// selected 为空表示执行扫描到的全部任务族；非空时只保留其中列出的 kind。
// 这里不认识任何供应商专属任务族，筛选语义完全由调用方（管理接口的兼容投影）决定。
func (s *Service) buildQueueItems(scans []accountScan, selected map[string]bool) []queueItem {
	items := make([]queueItem, 0, 64)
	seenRun := make(map[string]bool)
	for _, scan := range scans {
		for _, task := range scan.Tasks {
			if len(selected) > 0 && !selected[task.Kind] {
				continue
			}
			code := task.Code
			if task.RunCode != "" {
				// 聚合任务（例如开学季活动）只入队一次，并以 RunCode 执行。
				code = task.RunCode
				key := scan.UID + "\x00" + code
				if seenRun[key] {
					continue
				}
				seenRun[key] = true
			}
			progress := ""
			if task.Target > 0 {
				progress = fmt.Sprintf("%d/%d", task.Current, task.Target)
			}
			items = append(items, queueItem{
				UID: scan.UID, Label: scan.Label, Kind: task.Kind,
				Code: code, Status: "pending", Progress: progress,
			})
		}
	}
	return items
}

// runQueueItems 执行队列：账号内串行、账号间并发。
func (s *Service) runQueueItems(callbackID string, concurrency int, seq int64) {
	defer func() {
		s.queue.mu.Lock()
		if s.queue.seq == seq {
			s.queue.running = false
		}
		s.queue.mu.Unlock()
	}()

	ctx := context.Background()
	loaded := s.collectAccounts(ctx, callbackID, nil)
	byUID := make(map[string]*accountContext, len(loaded))
	for _, account := range loaded {
		byUID[account.uid()] = account
	}

	grouped := map[string][]int{}
	order := make([]string, 0, len(byUID))
	for index, item := range s.queueSnapshotItems() {
		if _, okGroup := grouped[item.UID]; !okGroup {
			order = append(order, item.UID)
		}
		grouped[item.UID] = append(grouped[item.UID], index)
	}

	type queueJob struct {
		account *accountContext
		indices []int
	}
	jobs := make([]queueJob, 0, len(order))
	for _, uid := range order {
		account, okAccount := byUID[uid]
		if !okAccount {
			s.markQueueItems(grouped[uid], "skipped", "账号已不可用", 0, 0)
			continue
		}
		jobs = append(jobs, queueJob{account: account, indices: grouped[uid]})
	}

	auth.RunBatch(jobs, concurrency, func(job queueJob) struct{} {
		target := job.account
		if errLock := s.lockAccount(ctx, target.uid()); errLock != nil {
			s.markQueueItems(job.indices, "skipped", "等待该账号的其它任务超时："+errLock.Error(), 0, 0)
			return struct{}{}
		}
		defer s.unlockAccount(target.uid())

		if preparer, okPreparer := core.As[core.TaskPreparer](target.vendor); okPreparer {
			if accepted := preparer.PrepareTasks(ctx, target.credential); accepted > 0 {
				logger.Debug("queue: accepted %d task(s) for %s", accepted, target.label())
				core.SleepContext(ctx, 1050*time.Millisecond)
			}
		}
		for _, index := range job.indices {
			item := s.queueItemAt(index)
			s.markQueueItem(index, "running", "")
			result, errRun := s.runTaskForAccount(ctx, target, item.Code)
			if errRun != nil {
				s.markQueueItem(index, "error", errRun.Error())
			} else {
				s.setQueueItemResult(index, result.Status, result.Message, result.Credit, result.Energy)
			}
			core.SleepContext(ctx, 1050*time.Millisecond)
		}
		return struct{}{}
	})
	logger.Info("task queue finished (seq=%d)", seq)
}

// runTaskForAccount 调用供应商的任务执行能力并记录结果。
func (s *Service) runTaskForAccount(ctx context.Context, account *accountContext, code string) (core.TaskRunResult, error) {
	runner, okRunner := core.As[core.TaskRunner](account.vendor)
	if !okRunner {
		return core.TaskRunResult{}, fmt.Errorf("该供应商不支持任务执行")
	}
	result, errRun := runner.RunTask(ctx, account.credential, code)
	if errRun != nil {
		s.recordTaskResult(account.uid(), code, errRun.Error(), "error", 0, 0)
		return core.TaskRunResult{}, errRun
	}
	s.recordTaskResult(account.uid(), code, result.Message, result.Status, result.Credit, result.Energy)
	return result, nil
}

// 队列状态访问器（避免在 goroutine 里长时间持锁）。

func (s *Service) queueSnapshotItems() []queueItem {
	s.queue.mu.Lock()
	defer s.queue.mu.Unlock()
	items := make([]queueItem, len(s.queue.items))
	copy(items, s.queue.items)
	return items
}

func (s *Service) queueItemAt(index int) queueItem {
	s.queue.mu.Lock()
	defer s.queue.mu.Unlock()
	if index < 0 || index >= len(s.queue.items) {
		return queueItem{}
	}
	return s.queue.items[index]
}

func (s *Service) markQueueItem(index int, status, message string) {
	s.queue.mu.Lock()
	defer s.queue.mu.Unlock()
	if index < 0 || index >= len(s.queue.items) {
		return
	}
	s.queue.items[index].Status = status
	if message != "" {
		s.queue.items[index].Message = message
	}
}

func (s *Service) setQueueItemResult(index int, status, message string, credit, energy int64) {
	s.queue.mu.Lock()
	defer s.queue.mu.Unlock()
	if index < 0 || index >= len(s.queue.items) {
		return
	}
	s.queue.items[index].Status = status
	s.queue.items[index].Message = message
	s.queue.items[index].Credit = credit
	s.queue.items[index].Energy = energy
}

func (s *Service) markQueueItems(indices []int, status, message string, credit, energy int64) {
	for _, index := range indices {
		s.setQueueItemResult(index, status, message, credit, energy)
	}
}

// recordTaskResult 记录任务执行结果到状态文件。
func (s *Service) recordTaskResult(uid, code, message, status string, credit, energy int64) {
	if strings.TrimSpace(uid) == "" {
		return
	}
	key := taskStateKey(uid, code)
	s.deps.MutateState(func(current *state.State) {
		current.Tasks[key] = state.TaskRecord{
			LastRunAt:   s.deps.Now().Format(time.RFC3339),
			LastStatus:  status,
			LastMessage: message,
			Credit:      credit,
			Energy:      energy,
		}
	})
}

type autoAllResult struct {
	Code     string `json:"task_code"`
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	Credit   int64  `json:"credit,omitempty"`
	Energy   int64  `json:"energy,omitempty"`
	Progress string `json:"progress,omitempty"`
}

// runAutoAll 调用供应商的一键完成，并记录每条结果。
func (s *Service) runAutoAll(ctx context.Context, account *accountContext) []autoAllResult {
	runner, okRunner := core.As[core.TaskRunner](account.vendor)
	if !okRunner {
		return nil
	}
	raw := runner.RunAllTasks(ctx, account.credential)
	results := make([]autoAllResult, 0, len(raw))
	for _, item := range raw {
		result := autoAllResult{
			Code: item.Code, Status: item.Status, Message: item.Message,
			Credit: item.Credit, Energy: item.Energy, Progress: item.Progress,
		}
		if !strings.HasPrefix(item.Code, "(") {
			s.recordTaskResult(account.uid(), item.Code, item.Message, item.Status, item.Credit, item.Energy)
		}
		results = append(results, result)
	}
	return results
}

// collectAccounts 收集本插件名下的可用账号；targets 为空表示全部。
func (s *Service) collectAccounts(ctx context.Context, callbackID string, targets []string) []*accountContext {
	accounts := make([]*accountContext, 0, 8)
	s.forEachAccount(ctx, callbackID, targets, func(_ context.Context, account *accountContext) error {
		accounts = append(accounts, account)
		return nil
	})
	return accounts
}

// findAccount 按标识查找账号（支持 auth_id / uid / auth_index）。
//
// Label 是展示名（昵称 / 邮箱），跨账号并不唯一：把它当成 ID 兜底会让
// 「删除账号」这类破坏性动作打到同名的另一个账号上。因此 Label 只在
// 全局唯一命中时才算数，命中多个就当找不到。
func (s *Service) findAccount(ctx context.Context, callbackID, identifier string) *accountContext {
	trimmed := strings.TrimSpace(identifier)
	if trimmed == "" {
		return nil
	}
	return pickAccount(s.collectAccounts(ctx, callbackID, nil), trimmed)
}

// pickAccount 从候选账号里按标识挑一个。
func pickAccount(candidates []*accountContext, identifier string) *accountContext {
	var labelMatch *accountContext
	labelHits := 0
	for _, account := range candidates {
		if account == nil {
			continue
		}
		if account.uid() == identifier || account.entry.ID == identifier ||
			account.entry.Name == identifier || account.entry.AuthIndex == identifier {
			return account
		}
		if account.entry.Label == identifier {
			labelMatch = account
			labelHits++
		}
	}
	if labelHits == 1 {
		return labelMatch
	}
	return nil
}

// activityStatusForAccounts 返回各账号的活动任务状态（供管理页展示）。
func (s *Service) activityStatusForAccounts(ctx context.Context, callbackID string) []map[string]any {
	loaded := s.collectAccounts(ctx, callbackID, nil)
	out := auth.RunBatch(loaded, queueMaxConcurrency, func(target *accountContext) map[string]any {
		entry := map[string]any{
			"uid":   target.uid(),
			"label": target.label(),
			"realm": target.vendor.Region(),
		}
		provider, okProvider := core.As[core.TaskStatusProvider](target.vendor)
		if !okProvider {
			entry["in_period"] = false
			entry["error"] = "该供应商没有活动任务状态"
			return entry
		}
		status := provider.TaskStatus(ctx, target.credential)
		entry["in_period"] = status.InPeriod
		if status.Error != "" {
			entry["error"] = status.Error
		}
		if len(status.Tasks) > 0 {
			list := make([]map[string]any, 0, len(status.Tasks))
			for _, task := range status.Tasks {
				list = append(list, map[string]any{
					"task_code": task.Code, "title": task.Title,
					"status": task.Status, "progress": task.Progress, "target": task.Target,
				})
			}
			entry["tasks"] = list
		}
		if status.Chances != nil {
			entry["chances"] = status.Chances
		}
		return entry
	})
	sort.Slice(out, func(i, j int) bool {
		return fmt.Sprint(out[i]["label"]) < fmt.Sprint(out[j]["label"])
	})
	return out
}

func taskStateKey(uid, code string) string {
	return strings.TrimSpace(uid) + "\x1f" + strings.TrimSpace(code)
}
