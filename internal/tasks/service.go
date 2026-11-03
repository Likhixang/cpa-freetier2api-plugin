package tasks

// 本文件是任务引擎的实例入口：调度循环、队列状态与账号锁。

import (
	"context"
	"sync"
	"time"
)

// Service 是一个 App 实例的任务引擎。
type Service struct {
	deps    Deps
	catalog *Catalog

	schedulerMu     sync.Mutex
	schedulerCancel context.CancelFunc
	schedulerWG     sync.WaitGroup
	running         bool
	pokeCh          chan string

	taskLockMu sync.Mutex
	taskLocks  map[string]chan struct{}
	queue      *queueState
	// autoAll 是「一键完成」的后台任务状态（见 autoall.go）。
	autoAll *autoAllJob

	// refreshGate 按账号串行化凭证续期（见 keepalive.go）。
	refreshGate refreshGate

	// jobMu/jobCtx/jobCancel 是后台动作（一键完成、异步任务族）的生命周期：
	// 插件关停时会 dlclose 动态库，若还有后台 goroutine 在打宿主回调就会崩。
	// Stop 先取消它，再给 jobWG 一个宽限期等 goroutine 自己退出。
	jobMu     sync.Mutex
	jobCtx    context.Context
	jobCancel context.CancelFunc
	jobWG     sync.WaitGroup
}

// jobStopGrace 是关停时等待后台动作收尾的宽限期。
const jobStopGrace = 5 * time.Second

// trackJob 在后台跑一个动作并登记到 jobWG，Stop 会等它结束。
//
// Add 必须与 Stop 的 Wait 互斥：两者都在 jobMu 下判断 jobCtx，Stop 先把
// jobCtx 置空再 Wait，因此不会出现「Wait 已开始又 Add」这种 WaitGroup 的
// 未定义用法。调度未启动时仍执行，但不登记（没有东西会等它）。
func (s *Service) trackJob(run func(ctx context.Context)) {
	s.jobMu.Lock()
	ctx, tracked := s.jobCtx, s.jobCtx != nil
	if tracked {
		s.jobWG.Add(1)
	}
	s.jobMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		if tracked {
			defer s.jobWG.Done()
		}
		run(ctx)
	}()
}

// New 构造任务引擎。
func New(deps Deps) *Service {
	return &Service{
		deps:      deps,
		catalog:   NewCatalog(deps.Registry()),
		pokeCh:    make(chan string, 4),
		taskLocks: map[string]chan struct{}{},
		queue:     &queueState{},
		autoAll:   &autoAllJob{},
	}
}

// Catalog 返回任务目录。
func (s *Service) Catalog() *Catalog { return s.catalog }

// Start 启动后台调度循环（幂等）。
func (s *Service) Start(ctx context.Context) {
	s.schedulerMu.Lock()
	defer s.schedulerMu.Unlock()
	if s.schedulerCancel != nil {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.schedulerCancel = cancel
	s.jobMu.Lock()
	jobCtx, jobCancel := context.WithCancel(context.Background())
	s.jobCtx, s.jobCancel = jobCtx, jobCancel
	s.jobMu.Unlock()
	s.schedulerWG.Add(1)
	go func() {
		defer s.schedulerWG.Done()
		s.runTaskScheduler(runCtx)
	}()
}

// Stop 停止后台调度循环与后台动作并等待退出。
func (s *Service) Stop() {
	s.schedulerMu.Lock()
	cancel := s.schedulerCancel
	s.schedulerCancel = nil
	s.schedulerMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.jobMu.Lock()
	jobCancel := s.jobCancel
	s.jobCtx, s.jobCancel = nil, nil
	s.jobMu.Unlock()
	if jobCancel != nil {
		jobCancel()
	}
	s.schedulerWG.Wait()
	// 后台动作可能正卡在上游 HTTP 上，取消不会立刻生效：给一个宽限期，
	// 超时就放手（宿主进程即将卸载插件，继续等只会拖慢关停）。
	done := make(chan struct{})
	go func() {
		s.jobWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(jobStopGrace):
	}
}

// Poke 唤醒调度循环重新计算下次执行时刻。
func (s *Service) Poke() {
	select {
	case s.pokeCh <- schedulerReconfigure:
	default:
	}
}

// Status 返回调度器状态（供管理页展示）。
func (s *Service) Status() map[string]any { return s.schedulerStatus() }

// EffectiveSettings 返回页面设置与配置合并后的可读视图。
func (s *Service) EffectiveSettings() map[string]any { return s.effectiveSettings() }

// RunAsync 在后台执行一个任务族。
func (s *Service) RunAsync(kind string) {
	s.trackJob(func(ctx context.Context) { s.runScheduledTaskContext(ctx, kind) })
}
