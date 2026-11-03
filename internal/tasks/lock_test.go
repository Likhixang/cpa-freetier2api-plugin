package tasks

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestTryLockAccountIsExclusive 确认同一账号同一时刻只能有一个任务持有锁。
//
// 签到与任务族共用这把锁：上游按「同账号并发会话」判风控，两个动作同时打过去
// 会随机回 429。
func TestTryLockAccountIsExclusive(t *testing.T) {
	service := &Service{}
	if !service.tryLockAccount("u1") {
		t.Fatal("首次抢锁应成功")
	}
	if service.tryLockAccount("u1") {
		t.Fatal("持锁期间再次抢锁应失败")
	}
	if !service.tryLockAccount("u2") {
		t.Fatal("不同账号之间不该互相阻塞")
	}
	service.unlockAccount("u1")
	if !service.tryLockAccount("u1") {
		t.Fatal("解锁后应能再次抢到")
	}
	service.unlockAccount("u1")
	service.unlockAccount("u2")
}

// TestTryLockAccountRejectsEmptyUID 确认没有账号身份时明确失败，而不是给出
// 「该账号正在执行其它任务」这种误导性的锁冲突文案。
func TestTryLockAccountRejectsEmptyUID(t *testing.T) {
	service := &Service{}
	if service.tryLockAccount("   ") {
		t.Fatal("空账号标识不该抢锁成功")
	}
}

// TestLockAccountWaitsAndHonoursContext 覆盖批量路径的排队语义：
// 锁被占用时排队等待，ctx 到期就返回错误，而不是跳过或永久卡住。
func TestLockAccountWaitsAndHonoursContext(t *testing.T) {
	service := &Service{}
	if !service.tryLockAccount("u1") {
		t.Fatal("预置持锁失败")
	}
	defer service.unlockAccount("u1")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	errLock := service.lockAccount(ctx, "u1")
	if !errors.Is(errLock, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", errLock)
	}
}

// TestLockAccountAcquiresAfterRelease 确认锁释放后排队者能拿到。
func TestLockAccountAcquiresAfterRelease(t *testing.T) {
	service := &Service{}
	if !service.tryLockAccount("u1") {
		t.Fatal("预置持锁失败")
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		service.unlockAccount("u1")
		close(released)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if errLock := service.lockAccount(ctx, "u1"); errLock != nil {
		t.Fatalf("排队抢锁失败：%v", errLock)
	}
	<-released
	service.unlockAccount("u1")
}

// TestUnlockAccountIsIdempotent 确认重复解锁不会 panic（关停路径会重复调用）。
func TestUnlockAccountIsIdempotent(t *testing.T) {
	service := &Service{}
	service.unlockAccount("u1")
	service.unlockAccount("u1")
	service.unlockAccount("")
}
