package tasks

import (
	"context"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/internal/core"
)

// panicTaskVendor 的 RunTask 在指定任务上 panic，其它任务正常完成。
type panicTaskVendor struct {
	rotatingVendor
	panicOn string
}

func (v *panicTaskVendor) RunTask(_ context.Context, _ *core.Credential, code string) (core.TaskRunResult, error) {
	if code == v.panicOn {
		var result *core.TaskRunResult
		return *result, nil // nil 解引用：运行时 panic
	}
	return core.TaskRunResult{Status: "done", Message: "ok " + code}, nil
}

func (v *panicTaskVendor) RunAllTasks(context.Context, *core.Credential) []core.AutoTaskResult {
	return nil
}

// TestRunQueueJobMarksRemainingItemsOnPanic：第二条任务 panic 后，第一条保留结果，
// 第二、三条标为失败并写明原因；账号锁已释放，后续任务还能拿到。
func TestRunQueueJobMarksRemainingItemsOnPanic(t *testing.T) {
	service := New(newFakeDeps(time.Now()))
	vendor := &panicTaskVendor{rotatingVendor: rotatingVendor{id: "queuetest"}, panicOn: "b"}
	account := &accountContext{
		entry:      core.HostAuth{ID: "queuetest-1"},
		vendor:     vendor,
		credential: &core.Credential{FileID: "queuetest-1"},
	}
	service.queue.items = []queueItem{
		{UID: "queuetest-1", Code: "a", Status: "pending"},
		{UID: "queuetest-1", Code: "b", Status: "pending"},
		{UID: "queuetest-1", Code: "c", Status: "pending"},
	}

	// 条目间有约 1 秒的固定间隔，这里只跑完第一条就 panic，耗时约 1 秒。
	service.runQueueJob(context.Background(), queueJob{account: account, indices: []int{0, 1, 2}})

	items := service.queueSnapshotItems()
	if items[0].Status != "done" || items[0].Message != "ok a" {
		t.Fatalf("finished item must keep its result: %+v", items[0])
	}
	for _, index := range []int{1, 2} {
		if items[index].Status != "error" || !strings.Contains(items[index].Message, "internal error in task queue") {
			t.Fatalf("item %d should be marked failed with the panic reason: %+v", index, items[index])
		}
	}

	lockCtx, lockCancel := context.WithTimeout(context.Background(), time.Second)
	defer lockCancel()
	if errLock := service.lockAccount(lockCtx, "queuetest-1"); errLock != nil {
		t.Fatalf("account lock must be released after panic: %v", errLock)
	}
	service.unlockAccount("queuetest-1")
}
