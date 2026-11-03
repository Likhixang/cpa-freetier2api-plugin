package admin

import (
	"strings"
	"testing"
)

// TestConsolePageRendersSections 是控制台页面的冒烟测试。
//
// 只断言「页面确实渲染出来了、关键区块和基础路径都在、没有残留模板占位」，
// 不锁 class 名与文案：那类快照断言重构一次就红一片，却抓不到任何真 bug。
func TestConsolePageRendersSections(t *testing.T) {
	const basePath = "/plugins/freetier2api"
	page := Page(basePath)

	if len(page) < 10000 {
		t.Fatalf("页面长度 %d，疑似渲染失败或资源缺失", len(page))
	}
	if strings.Contains(page, "{{") {
		t.Fatal("页面里残留模板占位符，说明注入未生效")
	}
	for _, marker := range []string{
		`id="view-accounts"`,
		`id="view-tasks"`,
		`id="view-settings"`,
		basePath,
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("页面缺少关键标记 %q", marker)
		}
	}
}
