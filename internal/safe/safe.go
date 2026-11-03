// Package safe 把 panic 挡在插件内部。
//
// 插件以动态库形式加载进 CPA 宿主进程：任何一个 goroutine 里未恢复的 panic
// 都会让整个宿主进程退出，而不只是让一次请求失败。因此
//   - 宿主 RPC 入口（cgo.go）把 panic 转成错误响应；
//   - 插件自己起的后台 goroutine（调度器、任务队列、流式转发、探测）在顶层
//     恢复 panic 并记一条带调用栈的错误日志，必要的收尾仍由各自的 defer 完成。
//
// 注意：recover 只在「被 defer 的函数本身」里调用才有效，所以 Recover 要直接
// 写成 defer safe.Recover(...)，不能再包一层闭包调用。
package safe

import (
	"fmt"
	"runtime/debug"

	"freetier2api-plugin/internal/logger"
)

// PanicError 是由 panic 转换来的错误，保留现场方便排查。
type PanicError struct {
	// Scope 是发生 panic 的位置描述（如 "rpc executor.execute_stream"）。
	Scope string
	// Value 是 panic 的原始参数。
	Value any
	// Stack 是恢复时的调用栈。
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("internal error in %s: panic: %v", e.Scope, e.Value)
}

// Unwrap 让 errors.Is / errors.As 能穿透到 panic(err) 传入的原始错误。
func (e *PanicError) Unwrap() error {
	if err, ok := e.Value.(error); ok {
		return err
	}
	return nil
}

// NewPanicError 记录一次已恢复的 panic 并返回对应的错误。
//
// 供必须自己写 recover 的调用方使用（例如恢复后还要把错误交给下游）：
//
//	defer func() {
//		if r := recover(); r != nil {
//			err = safe.NewPanicError("scope", r)
//		}
//	}()
func NewPanicError(scope string, value any) *PanicError {
	panicErr := &PanicError{Scope: scope, Value: value, Stack: debug.Stack()}
	logger.Error("recovered panic in %s: %v\n%s", scope, value, panicErr.Stack)
	return panicErr
}

// Recover 恢复当前 goroutine 的 panic 并记录日志。只能以 defer safe.Recover(scope) 的形式使用。
func Recover(scope string) {
	if r := recover(); r != nil {
		NewPanicError(scope, r)
	}
}

// Go 启动一个带 panic 恢复的 goroutine。
func Go(scope string, fn func()) {
	go func() {
		defer Recover(scope)
		fn()
	}()
}

// Call 同步执行 fn，把其中的 panic 转成 *PanicError 返回；fn 自己的错误原样返回。
func Call(scope string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = NewPanicError(scope, r)
		}
	}()
	return fn()
}
