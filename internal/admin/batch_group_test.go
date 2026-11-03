package admin

import (
	"reflect"
	"testing"
)

// TestGroupIndicesKeepsFirstSeenOrder 覆盖批量分组的两条不变量：
// 同 key 的下标归到一起，且 key 的顺序按首次出现排列。
//
// 批量动作的节奏问题全在分组是否正确（同厂商必须串行），真打上游反而测不出来。
func TestGroupIndicesKeepsFirstSeenOrder(t *testing.T) {
	cases := []struct {
		name        string
		keys        []string
		wantOrder   []string
		wantBuckets map[string][]int
	}{
		{
			name:        "空输入",
			keys:        nil,
			wantOrder:   []string{},
			wantBuckets: map[string][]int{},
		},
		{
			name:        "单厂商全部归一组",
			keys:        []string{"workbuddy", "workbuddy", "workbuddy"},
			wantOrder:   []string{"workbuddy"},
			wantBuckets: map[string][]int{"workbuddy": {0, 1, 2}},
		},
		{
			name:        "交错输入按首次出现排序",
			keys:        []string{"qoder", "trae", "qoder", "zcode", "trae"},
			wantOrder:   []string{"qoder", "trae", "zcode"},
			wantBuckets: map[string][]int{"qoder": {0, 2}, "trae": {1, 4}, "zcode": {3}},
		},
		{
			name:        "空 key 也自成一组",
			keys:        []string{"", "qoder", ""},
			wantOrder:   []string{"", "qoder"},
			wantBuckets: map[string][]int{"": {0, 2}, "qoder": {1}},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			order, buckets := groupIndices(testCase.keys)
			if !reflect.DeepEqual(order, testCase.wantOrder) {
				t.Fatalf("order = %v, want %v", order, testCase.wantOrder)
			}
			if !reflect.DeepEqual(buckets, testCase.wantBuckets) {
				t.Fatalf("buckets = %v, want %v", buckets, testCase.wantBuckets)
			}
		})
	}
}
