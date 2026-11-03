package core

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// TestIsRetryableFailure 锁定任务类调用的可重试集合。
//
// 429 必须算可重试：额度查询把它当限流直接展示，签到与任务族里它只是软限流，
// 一次就判失败会让账号白丢当天签到。
func TestIsRetryableFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"瞬时故障可重试", &Failure{Kind: FailureKindTransient}, true},
		{"软限流可重试", &Failure{Kind: FailureKindSoftRate}, true},
		{"限流可重试", &Failure{Kind: FailureKindRateLimit}, true},
		{"上游繁忙可重试", &Failure{Kind: FailureKindBusy}, true},
		{"5xx 可重试", &Failure{Kind: FailureKindServer}, true},
		{"参数错误不重试", &Failure{Kind: FailureKindBadParams}, false},
		{"内容拦截不重试", &Failure{Kind: FailureKindContentBlocked}, false},
		{"凭证失效不重试（由保活链路负责）", &Failure{Kind: FailureKindCredential}, false},
		{"普通错误不重试", errors.New("boom"), false},
		{"nil 不重试", nil, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := IsRetryableFailure(testCase.err); got != testCase.want {
				t.Fatalf("IsRetryableFailure = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestWithTaskRetrySucceedsWithoutRetry 确认首次成功不会产生任何等待。
func TestWithTaskRetrySucceedsWithoutRetry(t *testing.T) {
	calls := 0
	errRun := WithTaskRetry(context.Background(), func() error {
		calls++
		return nil
	})
	if errRun != nil {
		t.Fatalf("err = %v, want nil", errRun)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

// TestWithTaskRetryStopsOnDefinitiveError 确认不可重试的错误立刻上抛，不做退避。
func TestWithTaskRetryStopsOnDefinitiveError(t *testing.T) {
	calls := 0
	definitive := &Failure{Kind: FailureKindBadParams, Status: http.StatusBadRequest, Msg: "param is invalid"}
	errRun := WithTaskRetry(context.Background(), func() error {
		calls++
		return definitive
	})
	if !errors.Is(errRun, definitive) {
		t.Fatalf("err = %v, want 原始错误", errRun)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1（不可重试不该再打一次）", calls)
	}
}

// TestWithTaskRetryRetriesTransientFailure 确认可重试错误会再试一次并返回成功。
func TestWithTaskRetryRetriesTransientFailure(t *testing.T) {
	calls := 0
	errRun := WithTaskRetry(context.Background(), func() error {
		calls++
		if calls == 1 {
			return &Failure{Kind: FailureKindSoftRate, Status: http.StatusTooManyRequests, Msg: "slow down"}
		}
		return nil
	})
	if errRun != nil {
		t.Fatalf("err = %v, want nil", errRun)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

// TestWithTaskRetryHonoursCancel 确认 ctx 取消后不再继续退避重试。
func TestWithTaskRetryHonoursCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	errRun := WithTaskRetry(ctx, func() error {
		calls++
		cancel()
		return &Failure{Kind: FailureKindTransient, Msg: "eof"}
	})
	if !errors.Is(errRun, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", errRun)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1（取消后不该再打上游）", calls)
	}
}
