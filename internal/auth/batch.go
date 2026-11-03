package auth

// 本文件提供账号批处理的通用并发执行器。
// admin 的 quota/keepalive 与 tasks 队列共用，避免各自复制 semaphore/done 模式。

import (
	"sync"

	"freetier2api-plugin/internal/safe"
)

// RunBatch 以固定并发度执行 fn，并保持输入顺序返回结果。
//
// fn 里的 panic 在本工作协程内恢复并记日志，对应位置保留零值结果：插件是
// 宿主进程里的动态库，任何协程的 panic 都会让宿主退出。需要给出可读失败
// 结果的调用方应在 fn 内自行 recover（见 tasks 与 admin 的调用点）。
func RunBatch[T any, R any](items []T, concurrency int, fn func(T) R) []R {
	results := make([]R, len(items))
	if len(items) == 0 {
		return results
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > len(items) {
		concurrency = len(items)
	}
	semaphore := make(chan struct{}, concurrency)
	var waitGroup sync.WaitGroup
	for index := range items {
		waitGroup.Add(1)
		go func(slot int) {
			defer waitGroup.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			defer safe.Recover("batch item")
			results[slot] = fn(items[slot])
		}(index)
	}
	waitGroup.Wait()
	return results
}
