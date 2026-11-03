package auth

import "testing"

// 一个条目 panic 时，其它条目的结果照常保留、顺序不变，崩溃的位置是零值。
func TestRunBatchSurvivesPanickingItem(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	results := RunBatch(items, 2, func(item int) int {
		if item == 3 {
			panic("item 3 exploded")
		}
		return item * 10
	})
	want := []int{10, 20, 0, 40, 50}
	for index := range want {
		if results[index] != want[index] {
			t.Fatalf("results = %v, want %v", results, want)
		}
	}
}
