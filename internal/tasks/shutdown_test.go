package tasks

import (
	"context"
	"testing"
	"time"

	"freetier2api-plugin/internal/core"
)

// 关停语义：插件被卸载时宿主会 dlclose 动态库，后台动作如果还在打宿主
// 回调就会崩。Stop 必须先取消这些动作，并在宽限期内等它们退出。

// newLifecycleService 构造只带生命周期所需部件的最小 Service。
func newLifecycleService() *Service {
	return &Service{
		deps:      newFakeDeps(time.Now()),
		catalog:   NewCatalog(core.NewRegistry()),
		taskLocks: map[string]chan struct{}{},
		queue:     &queueState{},
		autoAll:   &autoAllJob{},
	}
}

// TestStopCancelsTrackedJobs 验证 Stop 会取消后台动作并等它退出。
func TestStopCancelsTrackedJobs(t *testing.T) {
	service := newLifecycleService()
	service.Start(context.Background())

	started := make(chan struct{})
	exited := make(chan struct{})
	service.trackJob(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(exited)
	})
	<-started

	begin := time.Now()
	service.Stop()
	if elapsed := time.Since(begin); elapsed > 2*time.Second {
		t.Fatalf("响应取消的后台动作不该拖慢关停，实际耗时 %v", elapsed)
	}
	select {
	case <-exited:
	default:
		t.Fatal("Stop 返回时后台动作应已退出")
	}
}

// TestStopGivesUpAfterGrace 验证不响应取消的动作不会让关停无限卡住。
func TestStopGivesUpAfterGrace(t *testing.T) {
	if testing.Short() {
		t.Skip("需要等满宽限期")
	}
	service := newLifecycleService()
	service.Start(context.Background())

	release := make(chan struct{})
	done := make(chan struct{})
	service.trackJob(func(context.Context) {
		defer close(done)
		<-release
	})

	begin := time.Now()
	service.Stop()
	elapsed := time.Since(begin)
	close(release)
	<-done

	if elapsed < jobStopGrace {
		t.Fatalf("应等满宽限期 %v，实际 %v", jobStopGrace, elapsed)
	}
	if elapsed > jobStopGrace+2*time.Second {
		t.Fatalf("宽限期结束后应立即放手，实际耗时 %v", elapsed)
	}
}

// TestStopWithoutStartIsSafe 验证未启动后台循环时 Stop 不 panic。
func TestStopWithoutStartIsSafe(t *testing.T) {
	service := newLifecycleService()
	service.Stop()
	service.Stop()
}
