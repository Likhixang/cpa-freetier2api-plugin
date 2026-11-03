package trae

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freetier2api-plugin/internal/core"
)

// 活动福利领取的行为测试。
//
// 这类端点的失败原因全写在 body 的 message 里，状态码经常还是 200：把
// 「已领取」当成「失败」会让人反复重试，把「活动已结束」当成「已领取」又会
// 掩盖真的没领到。所以归类逻辑必须逐个码位固定下来。

// ugTestCredential 造一个国内版凭证（uid 决定派生的设备头）。
func ugTestCredential() *Credential {
	return &Credential{
		Vendor:      VendorIDCN,
		Region:      RegionCN,
		AccessToken: "test-access-token",
		UID:         "2487573687317868",
	}
}

// ugTestConfig 指向假上游。
//
// 插件不发真实请求，一律经宿主 http.do 回调，所以用例必须先 stubHost。
func ugTestConfig(t *testing.T, host string) Config {
	t.Helper()
	return testConfig(t, host)
}

// TestUGActionsAreBatchable 验证：批量入口都是「无需输入、可直接 POST」的。
//
// 兑换码需要用户提供内容，而手动触发路由只传任务族名、没有请求体入口，所以
// 那条链路已删除——留着它只会是一个永远调不到的实现。
//
// cobrand/claim 与两个 share_link 需要活动 ID / 作品 ID（上游实测回
// 4001 / 9080「param is invalid」），插件取不到：它们仍登记在动作表里并标记
// needs_input，但不得出现在批量入口——每轮白打 3 次注定失败的请求，只会拖慢
// 整轮并把真实原因糊成「暂不可领」。
func TestUGActionsAreBatchable(t *testing.T) {
	actions := UGActions()
	if len(actions) != 2 {
		t.Fatalf("活动入口数 = %d, 期望 2（竞赛奖励 + 作品抽奖）", len(actions))
	}
	// 需要入参的端点不得进批量入口，这里用路径字面量做黑名单，
	// 防止日后有人凭记忆把它们加回批量入口。
	for _, excluded := range []string{
		"/trae/api/v2/ug/cobrand/claim",
		"/trae/api/v2/ug/work_fission/share_link",
		"/trae/api/v2/ug/code_fission/share_link",
	} {
		for _, action := range actions {
			if action.Path == excluded {
				t.Errorf("需要入参的端点不该进批量入口: %s", excluded)
			}
		}
	}
	// 预算必须严格大于最坏耗时。最坏耗时按真实链路重算：
	//   固定端点 N×perRequestTimeout + activity/info 1×ugDynamicTimeout
	//   + 活动触发 M×ugDynamicTimeout + 裂变整块 ugFissionReserve()
	// M 不写死，用触发门限（ugFissionReserve()+ugClaimReserve）从预算反推，
	// 这样改超时/加端点时这里的验算会自动跟着变，不会漏改。
	if ugFissionReserve() != 3*ugDynamicTimeout {
		t.Fatalf("ugFissionReserve() = %v，裂变是 share_status/share_link/grant 三次请求，应为 3×%v",
			ugFissionReserve(), ugDynamicTimeout)
	}
	fixedWorst := time.Duration(len(actions)) * perRequestTimeout
	gate := ugFissionReserve() + ugClaimReserve
	remaining := ugRoundTimeout() - fixedWorst - ugDynamicTimeout
	triggers := 0
	for remaining >= gate {
		triggers++
		remaining -= ugDynamicTimeout
	}
	if triggers < 1 {
		t.Fatalf("按门限 %v 反推触发次数为 0：活动表根本领不到东西", gate)
	}
	worst := fixedWorst + ugDynamicTimeout + time.Duration(triggers)*ugDynamicTimeout + ugFissionReserve()
	if ugRoundTimeout() <= worst {
		t.Fatalf("ugRoundTimeout() = %v，必须大于最坏耗时 %v（固定 %v + 表 %v + 触发 %d×%v + 裂变 %v）",
			ugRoundTimeout(), worst, fixedWorst, ugDynamicTimeout, triggers, ugDynamicTimeout, ugFissionReserve())
	}
	// 宿主上限：手动触发走 admin 的 3min 请求超时，超了等于白跑。
	if ugRoundTimeout() >= 3*time.Minute {
		t.Fatalf("ugRoundTimeout() = %v，不能达到宿主的 3min 请求上限", ugRoundTimeout())
	}
	if ugClaimReserve <= ugDynamicTimeout {
		t.Fatalf("ugClaimReserve = %v，必须大于一次动态端点超时 %v，否则会反复起必然超时的请求",
			ugClaimReserve, ugDynamicTimeout)
	}
	seen := map[string]bool{}
	for _, action := range actions {
		if action.Path == "" || action.Label == "" || action.Key == "" {
			t.Errorf("%s: key/path/label 不能为空", action.Key)
		}
		if seen[action.Key] {
			t.Errorf("入口 key 重复: %s", action.Key)
		}
		seen[action.Key] = true
	}
}

// TestClaimUGClassifiesEachEndpoint 验证：逐个端点的归类。
func TestClaimUGClassifiesEachEndpoint(t *testing.T) {
	type reply struct {
		status int
		body   string
	}
	// 批量入口只剩两个端点，所以分两轮跑：一轮覆盖「领到 / 活动关闭」，
	// 一轮覆盖「已领取 / 缺参」。第二轮在 lottery 上借用 share_link 的
	// 缺参码 4001，是为了把 ugParamMarkers 那条路径也走到（归类逻辑同源，
	// 与端点无关）。
	rounds := []struct {
		name    string
		replies map[string]reply
		want    map[string]UGStatus
	}{
		{
			name: "正常发放",
			replies: map[string]reply{
				EpUGCompetitionClaim: {200, `{"code":0,"message":"ok"}`},
				EpUGWorkLotteryDraw:  {200, `{"code":2002,"message":"活动已结束"}`},
			},
			want: map[string]UGStatus{
				"competition_claim": UGClaimed,
				"work_lottery_draw": UGUnavailable,
			},
		},
		{
			name: "已领取与缺参",
			replies: map[string]reply{
				EpUGCompetitionClaim: {200, `{"code":1001,"message":"该奖励已领取"}`},
				EpUGWorkLotteryDraw:  {200, `{"code":4001,"message":"param is invalid"}`},
			},
			want: map[string]UGStatus{
				"competition_claim": UGAlready,
				"work_lottery_draw": UGUnavailable,
			},
		},
	}

	for _, round := range rounds {
		replies := round.replies
		stubHost(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ugNeutralEndpoint(w, r) {
				return
			}
			if r.Method != http.MethodPost {
				t.Errorf("%s: method = %s, 期望 POST", r.URL.Path, r.Method)
			}
			if got := r.Header.Get("Authorization"); got != "Cloud-IDE-JWT test-access-token" {
				t.Errorf("%s: Authorization = %q", r.URL.Path, got)
			}
			if got := r.Header.Get("X-Market-User-Id"); got == "" {
				t.Errorf("%s: 缺少派生的 X-Market-User-Id", r.URL.Path)
			}
			item, ok := replies[r.URL.Path]
			if !ok {
				t.Errorf("批量领取打到了未知端点 %s", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(item.status)
			_, _ = w.Write([]byte(item.body))
		}))

		results, errClaim := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
		server.Close()
		if errClaim != nil {
			t.Fatalf("%s: ClaimUG 返回错误: %v", round.name, errClaim)
		}
		if len(results) != 2 {
			t.Fatalf("%s: 结果数 = %d, 期望 2", round.name, len(results))
		}
		for _, result := range results {
			status, known := round.want[result.Key]
			if !known {
				t.Errorf("%s: 出现预期外的入口 key: %s", round.name, result.Key)
				continue
			}
			if result.Status != status {
				t.Errorf("%s: %s status = %d, 期望 %d（文案 %q）",
					round.name, result.Key, result.Status, status, result.Message)
			}
		}
	}
}

// TestClaimUGAbortsOnCancelledContext 验证：取消后不再继续打后面的端点。
func TestClaimUGAbortsOnCancelledContext(t *testing.T) {
	stubHost(t)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results, errClaim := ClaimUG(ctx, ugTestCredential(), ugTestConfig(t, server.URL))
	if errClaim == nil {
		t.Fatal("取消后应返回错误")
	}
	if calls != 0 {
		t.Errorf("取消后仍打了 %d 个端点", calls)
	}
	if len(results) != 0 {
		t.Errorf("取消后不应有结果，得到 %d 条", len(results))
	}
}

// TestClaimUGRejectsMissingIdentity 验证：没有 uid 的凭证不派发共享设备身份。
func TestClaimUGRejectsMissingIdentity(t *testing.T) {
	credential := ugTestCredential()
	credential.UID = ""
	if _, errClaim := ClaimUG(context.Background(), credential, ugTestConfig(t, "https://example.invalid")); errClaim == nil {
		t.Fatal("缺少 uid 时应报错")
	}
}

// TestClaimUGRejectsGlobalRegion 验证：国际版没有这套活动。
func TestClaimUGRejectsGlobalRegion(t *testing.T) {
	credential := ugTestCredential()
	credential.Region = RegionGlobal
	if _, errClaim := ClaimUG(context.Background(), credential, ConfigFor(RegionGlobal)); errClaim == nil {
		t.Fatal("国际版应拒绝活动福利领取")
	}
}

// TestClaimUGTreatsNonJSONAsFailure 锁住「200 + 非 JSON ≠ 已领取」。
//
// 网关超时页、空 body、{"data":{}} 都没有 code 字段；按零值当成 code=0 会把
// 失败报成「已领取」，用户以为领到了，实际什么都没有。
func TestClaimUGTreatsNonJSONAsFailure(t *testing.T) {
	for _, body := range []string{"<html>502 Bad Gateway</html>", "", `{"data":{}}`} {
		stubHost(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ugNeutralEndpoint(w, r) {
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
		}))
		results, errClaim := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
		server.Close()
		if errClaim != nil {
			t.Fatalf("body=%q: 不该中止整轮: %v", body, errClaim)
		}
		for _, result := range results {
			if result.Status != UGFailed {
				t.Fatalf("body=%q: %s status = %d, 期望 UGFailed", body, result.Key, result.Status)
			}
			if strings.TrimSpace(result.Message) == "" {
				t.Fatalf("body=%q: %s 失败时必须带原因", body, result.Key)
			}
		}
	}
}

// TestClaimUGStopsOnUnauthorized 锁住「401 立刻停」：后面的端点只会得到同一个
// 401，继续打只会让日志更难读；已拿到的结果仍要带出去。
func TestClaimUGStopsOnUnauthorized(t *testing.T) {
	stubHost(t)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":1001,"message":"unauthorized"}`))
	}))
	defer server.Close()

	results, errClaim := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if errClaim == nil {
		t.Fatal("401 应中止整轮并上报错误")
	}
	if calls != 1 {
		t.Fatalf("401 后不应继续请求，实际打了 %d 个端点", calls)
	}
	if len(results) != 1 || results[0].Status != UGFailed {
		t.Fatalf("应带回首个失败结果: %+v", results)
	}
}

// TestSummarizeUGCounts 验证：汇总文案的四类计数与逐条文案。
func TestSummarizeUGCounts(t *testing.T) {
	results := []UGResult{
		{Key: "a", Label: "竞赛奖励", Status: UGClaimed},
		{Key: "b", Label: "联合活动奖励", Status: UGAlready},
		{Key: "c", Label: "作品抽奖", Status: UGUnavailable, Message: "活动已结束"},
		{Key: "d", Label: "作品分享奖励", Status: UGFailed, Message: "HTTP 500"},
	}
	text := SummarizeUG(results)
	// 「活动已结束」属于有信息量的一档，逐条展示而不是压成计数。
	for _, want := range []string{"领到 1 项", "此前已领 1 项", "失败 1 项", "竞赛奖励：已领取", "作品抽奖：暂不可领（活动已结束）"} {
		if !strings.Contains(text, want) {
			t.Errorf("汇总文案缺少 %q：%s", want, text)
		}
	}
	if strings.Contains(text, "暂不可领 ") {
		t.Errorf("没有可压缩项时不该出现计数：%s", text)
	}
	if got := SummarizeUG(nil); got != "没有可领取的活动入口" {
		t.Errorf("空结果文案 = %q", got)
	}
}

// TestSummarizeUGCompactedCountMatchesNames 锁住「计数只数被压缩的那些」。
// 之前计数把逐条展示的也算进去，于是文案会出现「暂不可领 3 项（邀请码 X）」
// 甚至「暂不可领 1 项（）」这种括号里空着的错位。
func TestSummarizeUGCompactedCountMatchesNames(t *testing.T) {
	text := SummarizeUG([]UGResult{
		{Key: "a", Label: "作品抽奖", Status: UGUnavailable, Message: "当前不可领"},
		{Key: "b", Label: "作品分享奖励", Status: UGUnavailable, Message: "当前不可领"},
		{Key: "c", Label: "邀请奖励", Status: UGUnavailable, Message: "邀请码 6DXPMKYC5V23，已邀请 3 人，其中 2 人待发放"},
	})
	if !strings.Contains(text, "暂不可领 2 项（作品抽奖、作品分享奖励）") {
		t.Fatalf("压缩计数与名字列表必须一致：%s", text)
	}
	if !strings.Contains(text, "邀请码 6DXPMKYC5V23") {
		t.Fatalf("邀请码必须逐条展示：%s", text)
	}
	if strings.Contains(text, "（）") || strings.Contains(text, "()") {
		t.Fatalf("不允许出现空括号：%s", text)
	}
}

// TestSummarizeUGAlreadyKeepsProgress 锁住「已领取也要带出邀请进度」。
func TestSummarizeUGAlreadyKeepsProgress(t *testing.T) {
	text := SummarizeUG([]UGResult{
		{Key: "invite", Label: "邀请奖励", Status: UGAlready, Message: "已邀请 5 人，其中 3 人待发放"},
	})
	if !strings.Contains(text, "已邀请 5 人") {
		t.Fatalf("已领取分支不能丢掉邀请进度：%s", text)
	}
}

// 暂不可领的活动每轮都一样，只列名字，不逐条铺明细。
func TestSummarizeUGKeepsUnavailableCompact(t *testing.T) {
	text := SummarizeUG([]UGResult{
		{Key: "a", Label: "竞赛奖励", Status: UGUnavailable, Message: "活动暂不可用"},
		{Key: "b", Label: "作品抽奖", Status: UGUnavailable, Message: "活动暂不可用"},
	})
	for _, want := range []string{"暂不可领 2 项", "（竞赛奖励、作品抽奖）"} {
		if !strings.Contains(text, want) {
			t.Errorf("汇总文案缺少 %q：%s", want, text)
		}
	}
	if strings.Contains(text, "活动暂不可用") {
		t.Errorf("暂不可领的明细不该铺进汇总：%s", text)
	}
}

// TestClassifyUGMessage 固定归类口径：码表未知，只按文案粗分。
func TestClassifyUGMessage(t *testing.T) {
	cases := []struct {
		message string
		want    UGStatus
	}{
		{"该奖励已领取", UGAlready},
		{"你已经参与过该活动", UGAlready},
		{"活动已结束", UGUnavailable},
		{"活动未开始", UGUnavailable},
		{"当前账号不满足参与条件", UGUnavailable},
		{"活动暂不可用", UGUnavailable},
		{"param is invalid", UGUnavailable},
		{"参数错误", UGUnavailable},
		{"", UGFailed},
	}
	for _, item := range cases {
		if got := classifyUGMessage(item.message); got != item.want {
			t.Errorf("classifyUGMessage(%q) = %d, 期望 %d", item.message, got, item.want)
		}
	}
}

// TestTaskSpecsFollowRegion 验证：只有国内版声明活动福利族，且内容与手动族语义一致。
//
// 宿主把 TaskSpec 原样交给控制台渲染：Label / Description 写错，用户看到的就是
// 错的；Route 丢了，手动触发入口整个消失（admin 跳过 Route 为空的手动族）；
// Manual 丢了，一个「不是每天都有东西可领」的活动会被排程每天空打。
//
// 不带 DefaultHours 是刻意的：这些入口只在活动期有东西可领，默认排程只会每天
// 白打 2 次空请求。
func TestTaskSpecsFollowRegion(t *testing.T) {
	specs := (&adapter{config: ConfigFor(RegionCN)}).TaskSpecs()
	if len(specs) != 1 {
		t.Fatalf("国内版任务族数 = %d, 期望 1", len(specs))
	}
	spec := specs[0]
	if spec.Kind != TaskKindUG {
		t.Errorf("Kind = %q, 期望 %q", spec.Kind, TaskKindUG)
	}
	if TaskKindUG != "ug" {
		t.Errorf("族名 = %q, 期望与相邻 vendor 一致的 %q", TaskKindUG, "ug")
	}
	if spec.Label != "活动福利领取" {
		t.Errorf("Label = %q（控制台原样展示）", spec.Label)
	}
	if !strings.Contains(spec.Description, "活动中心") || !strings.Contains(spec.Description, "分享裂变") {
		t.Errorf("Description = %q（控制台原样展示，改文案要同步这里）", spec.Description)
	}
	if spec.Route != "/ug/claim" {
		t.Errorf("Route = %q, 期望 /ug/claim（缺了控制台不出现手动触发入口）", spec.Route)
	}
	if !spec.Manual {
		t.Error("Manual = false，活动福利是手动族")
	}
	if len(spec.DefaultHours) != 0 {
		t.Errorf("手动族不应带默认排程，得到 %v", spec.DefaultHours)
	}

	// 国际版没有这套活动：返回 nil，而不是一个空族。
	if global := (&adapter{config: ConfigFor(RegionGlobal)}).TaskSpecs(); global != nil {
		t.Errorf("国际版任务族 = %v, 期望 nil", global)
	}
}

// TestRunScheduledRejectsUnknownKind 验证：未知任务族直接报错，不静默成功。
func TestRunScheduledRejectsUnknownKind(t *testing.T) {
	cn := &adapter{config: ConfigFor(RegionCN)}
	if _, errRun := cn.RunScheduled(context.Background(), nil, "workbuddy_dailybonus"); errRun == nil {
		t.Fatal("未知任务族应报错")
	}
}

// TestClaimUGReadsNoCodeState 锁住「无业务码响应按状态字段归类」。
//
// 实测 claim_package 回 {"eligible":false,...}：没有 code。按 code==nil 一律
// 归失败，会把「当前账号不满足条件」报成上游故障，用户只能看到一串无意义的
// 失败横幅。
//
// 每个用例连文案一起断言：三个判定分支（不满足条件 / 可参与但未发放 / 本次
// 未发放）是用户唯一能看到的输出，改判定时文案错配比状态错更难发现。
func TestClaimUGReadsNoCodeState(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		status  UGStatus
		message string
	}{
		{"资格不足-顶层", `{"eligible":false,"message":"no quota"}`, UGUnavailable, "当前账号不满足活动条件"},
		{"资格不足-嵌套", `{"data":{"eligible":false}}`, UGUnavailable, "当前账号不满足活动条件"},
		{"本次已发放", `{"eligible":true,"newly_granted":true}`, UGClaimed, "上游本次发放成功"},
		{"可参与但未发放", `{"eligible":true,"newly_granted":false}`, UGUnavailable, "活动可参与，但本次未发放"},
		// 两个字段同时出现且互相矛盾：以「本次发放成功」为准。上游真的发了
		// 东西，却因为 eligible=false 报「不满足条件」，等于把领到的奖励吞掉。
		{"资格不足但本次已发放", `{"eligible":false,"newly_granted":true}`, UGClaimed, "上游本次发放成功"},
		// 只给 newly_granted=false，没有 eligible：本次没发东西，但不是
		// 「不满足条件」，文案不能张冠李戴。
		{"仅本次未发放", `{"newly_granted":false}`, UGUnavailable, "本次未发放"},
		{"本次已发放-嵌套", `{"data":{"eligible":false,"newly_granted":true}}`, UGClaimed, "上游本次发放成功"},
		// 混合形状：顶层只给一个字段、data 层另给一个。必须逐字段回落补齐——
		// 「顶层有任一字段就整层信顶层」会把 data 里的 newly_granted 丢掉，
		// 把已发放读成「可参与但本次未发放」。
		{"混合-顶层eligible+data发放", `{"eligible":true,"data":{"newly_granted":true}}`, UGClaimed, "上游本次发放成功"},
		{"混合-顶层未发放+data不满足", `{"newly_granted":false,"data":{"eligible":false}}`, UGUnavailable, "当前账号不满足活动条件"},
	}
	for _, item := range cases {
		stubHost(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ugNeutralEndpoint(w, r) {
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(item.body))
		}))
		results, errClaim := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
		server.Close()
		if errClaim != nil {
			t.Fatalf("%s: 不该中止整轮: %v", item.name, errClaim)
		}
		for _, result := range results {
			if result.Status != item.status {
				t.Fatalf("%s: %s status = %d, 期望 %d", item.name, result.Key, result.Status, item.status)
			}
			if result.Message != item.message {
				t.Fatalf("%s: %s message = %q, 期望 %q", item.name, result.Key, result.Message, item.message)
			}
			// 这条路径上游没给业务码，Code 是零值占位——所以只能按 Status 判，
			// 不能按 Code==0 判「上游回 0 = 已领取」。
			if result.Code != 0 {
				t.Fatalf("%s: %s Code = %d, 这条路径不该有业务码", item.name, result.Key, result.Code)
			}
		}
	}
}

// TestClaimUGClassifiesMissingParamOnNon200 锁住「非 200 的缺参数也归暂不可领」。
//
// 实测 cobrand/claim 缺 activity_id 回 400 + code 9080「param is invalid」。
func TestClaimUGClassifiesMissingParamOnNon200(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ugNeutralEndpoint(w, r) {
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":9080,"message":"param is invalid"}`))
	}))
	defer server.Close()

	results, errClaim := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if errClaim != nil {
		t.Fatalf("400 不该中止整轮: %v", errClaim)
	}
	for _, result := range results {
		if result.Status != UGUnavailable {
			t.Fatalf("%s status = %d, 期望 UGUnavailable（文案 %q）", result.Key, result.Status, result.Message)
		}
		if result.Code != 9080 {
			t.Errorf("%s code = %d, 期望原样带出 9080", result.Key, result.Code)
		}
	}
}

// TestClaimUGKeepsServerErrorsAsFailure 锁住「5xx / 429 不能被文案折成暂不可领」。
//
// 这是本函数原先的缺陷：非 200 分支只要文案命中「暂不可用」就归 UGUnavailable，
// 于是一个 503（上游真的挂了）会被显示成「活动暂不可领」，调度不会重试、用户
// 也不会，故障被静默吞掉。归类只对 4xx 成立；5xx 与 429 必须留给 Classify。
func TestClaimUGKeepsServerErrorsAsFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"503-文案像活动关闭", http.StatusServiceUnavailable, `{"code":5001,"message":"活动暂不可用"}`},
		{"503-纯网关页", http.StatusBadGateway, `<html>502 Bad Gateway</html>`},
		{"500-缺参文案", http.StatusInternalServerError, `{"code":9080,"message":"param is invalid"}`},
		{"429-已领取文案", http.StatusTooManyRequests, `{"code":1001,"message":"该奖励已领取"}`},
	}
	for _, item := range cases {
		stubHost(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ugNeutralEndpoint(w, r) {
				return
			}
			w.WriteHeader(item.status)
			_, _ = w.Write([]byte(item.body))
		}))
		results, errClaim := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
		server.Close()
		if errClaim != nil {
			t.Fatalf("%s: 不该中止整轮: %v", item.name, errClaim)
		}
		if len(results) != 2 {
			t.Fatalf("%s: 结果数 = %d, 期望 2", item.name, len(results))
		}
		for _, result := range results {
			if result.Status != UGFailed {
				t.Errorf("%s: %s status = %d, 期望 UGFailed（文案 %q）", item.name, result.Key, result.Status, result.Message)
			}
			if strings.TrimSpace(result.Message) == "" {
				t.Errorf("%s: %s 失败时必须带原因", item.name, result.Key)
			}
		}
	}
}

// TestClaimUGClassifiesAlreadyOnNon200 锁住「4xx 的已领取也归已领取」。
//
// 实测幂等端点对重复领取回 409 + 1001「该奖励已领取」。只认 UGUnavailable
// 会让这种正常结果掉进「失败」，用户看到红字却什么都没坏。
func TestClaimUGClassifiesAlreadyOnNon200(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ugNeutralEndpoint(w, r) {
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":1001,"message":"该奖励已领取"}`))
	}))
	defer server.Close()

	results, errClaim := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if errClaim != nil {
		t.Fatalf("409 不该中止整轮: %v", errClaim)
	}
	for _, result := range results {
		if result.Status != UGAlready {
			t.Errorf("%s status = %d, 期望 UGAlready（文案 %q）", result.Key, result.Status, result.Message)
		}
		if result.Code != 1001 {
			t.Errorf("%s code = %d, 期望原样带出 1001", result.Key, result.Code)
		}
	}
}

// TestReadUGState 验证：没有状态字段时必须退回失败归类，不能被当成可领取。
func TestReadUGState(t *testing.T) {
	for _, body := range []string{`{"data":{}}`, `<html>502</html>`, `{}`, `null`} {
		if _, okState := readUGState([]byte(body)); okState {
			t.Errorf("body=%q: 不该识别出状态字段", body)
		}
	}
}

// TestUGDetailText 验证：机器文案翻成中文提示，认不出的原样返回。
//
// 只覆盖可达输入：需要活动/作品 ID 的三个端点已不在批量入口里，缺参类文案
// （param is invalid / 9080）不会再出现在结果中，所以那条翻译分支已删除。
// 这里仍保留一条「不可达也不能乱翻」的断言，防止有人日后顺手加回翻译。
func TestUGDetailText(t *testing.T) {
	cases := map[string]string{
		"活动暂不可用":           "活动暂不可用",
		"活动暂未开放":           "活动暂不可用",
		"HTTP 500 服务繁忙":    "HTTP 500 服务繁忙",
		"param is invalid": "param is invalid",
	}
	for message, want := range cases {
		if got := ugDetailText(message); got != want {
			t.Errorf("ugDetailText(%q) = %q, 期望 %q", message, got, want)
		}
	}
}

// ugNeutralEndpoint 让「只看固定端点」的用例不受动态入口干扰。
//
// ClaimUG 现在还会读活动表（activity/info）并探测裂变期（code_fission/
// share_status）。这两个入口有各自的用例；这里给它们一份「空表 + 这期不存在」
// 的默认回复，避免同一份 body 同时喂给三个端点、把断言搅成说不清哪条路径失败。
func ugNeutralEndpoint(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case EpUGActivityInfo:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"commercial_activities":[]}`))
		return true
	case EpCodeFissionShareStatus:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":9090,"message":"活动暂不可用"}`))
		return true
	}
	return false
}

// ugCallLog 记录假上游收到的路径。handler 跑在独立 goroutine 里，加锁是必需的。
type ugCallLog struct {
	mu    sync.Mutex
	paths []string
}

func (l *ugCallLog) add(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paths = append(l.paths, path)
}

func (l *ugCallLog) count(path string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, p := range l.paths {
		if p == path {
			n++
		}
	}
	return n
}

// ugStub 造一个假上游：routes 里有的路径回给定 body，其余走中性回复。
func ugStub(routes map[string]string, log *ugCallLog) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if log != nil {
			log.add(r.URL.Path)
		}
		if body, ok := routes[r.URL.Path]; ok {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
			return
		}
		if ugNeutralEndpoint(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"message":"ok"}`))
	}
}

// ugResultByKey 按 key 索引结果，key 重复直接判失败。
func ugResultByKey(t *testing.T, results []UGResult) map[string]UGResult {
	t.Helper()
	byKey := make(map[string]UGResult, len(results))
	for _, result := range results {
		if _, dup := byKey[result.Key]; dup {
			t.Fatalf("结果 key 重复：%s", result.Key)
		}
		byKey[result.Key] = result
	}
	return byKey
}

// 活动表的五条覆盖了真实响应的全部形态：已发放、可领、自动发放、签到、已结束。
const ugActivityTableFixture = `{"commercial_activities":[
	{"activity_id":"new_user_202609","activity_type":102,"enabled":true,"work_extra":{"granted":true,"grantable":false}},
	{"activity_id":"checkin_202610","activity_type":104,"enabled":true,"work_extra":{"granted":false,"grantable":true}},
	{"activity_id":"send_msg_202610","activity_type":105,"enabled":true,"work_extra":{"auto_grant":true,"granted":false,"grantable":false}},
	{"activity_id":"student_202610","activity_type":107,"enabled":true,"work_extra":{"granted":false,"grantable":true,"general_credits":500}},
	{"activity_id":"ended_202608","activity_type":103,"enabled":true,"end_time_ms":1700000000000,"work_extra":{"granted":false,"grantable":true}}
]}`

// 实测 student_auth_reward 在 activity/info 里 grantable=true，但 action 只回
// 200 `{}`、复查 granted 仍是 false —— 上游要的是先完成学籍认证。这类「要用户
// 自己去认证 / 装桌面端 / 发消息」的活动一律不打 action，也不当成福利推出来。
func TestClaimUGSkipsActivitiesNeedingManualAction(t *testing.T) {
	stubHost(t)
	log := &ugCallLog{}
	server := httptest.NewServer(ugStub(map[string]string{
		EpUGActivityInfo:   ugActivityTableFixture,
		EpUGActivityAction: `{"code":0,"message":"发放成功"}`,
	}, log))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err != nil {
		t.Fatalf("ClaimUG 出错：%v", err)
	}
	if got := log.count(EpUGActivityAction); got != 0 {
		t.Fatalf("activity/action 调了 %d 次，want 0（学生认证/发消息都要用户自己动手，插件不该打）", got)
	}
	byKey := ugResultByKey(t, results)
	for _, key := range []string{"activity:student_202610", "activity:send_msg_202610"} {
		if _, ok := byKey[key]; ok {
			t.Fatalf("%s 需要用户自己动手，不该出现在结果里", key)
		}
	}
	if r, ok := byKey["activity:new_user_202609"]; !ok || r.Status != UGAlready {
		t.Fatalf("已发放的新人活动应报已领取：%+v", r)
	}
	if r, ok := byKey["activity:checkin_202610"]; !ok || !strings.Contains(r.Message, "签到") {
		t.Fatalf("签到类活动应指向每日签到：%+v", r)
	}
	if _, ok := byKey["activity:ended_202608"]; ok {
		t.Fatal("已结束的活动不该出现在结果里")
	}
}

// 一键能领的必须领干净：不按固定条数截断，一轮里有几条可领就领几条。
func TestClaimUGClaimsEveryClaimableActivity(t *testing.T) {
	stubHost(t)
	log := &ugCallLog{}
	server := httptest.NewServer(ugStub(map[string]string{
		EpUGActivityInfo:   ugClaimableTableFixture,
		EpUGActivityAction: `{"code":0,"message":"发放成功"}`,
	}, log))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err != nil {
		t.Fatalf("ClaimUG 出错：%v", err)
	}
	if got := log.count(EpUGActivityAction); got != 3 {
		t.Fatalf("activity/action 调了 %d 次，want 3（三条都可一键领）", got)
	}
	claimed := 0
	for _, result := range results {
		// 固定端点那两条也算领到，这里只看活动表驱动的三条。
		if result.Status == UGClaimed && strings.HasPrefix(result.Key, "activity:") {
			claimed++
		}
	}
	if claimed != 3 {
		t.Fatalf("活动领到 %d 项，want 3：%+v", claimed, results)
	}
	// 未知类型的展示名兜底成 activity_id，不能出现空标签或「未知活动」之外的裸空。
	if label := ugResultByKey(t, results)["activity:promo_a"].Label; !strings.Contains(label, "promo_a") {
		t.Fatalf("未知类型的展示名应兜底成 activity_id，得到 %q", label)
	}
}

// 这里的 999 是刻意的不存在值：上游已知类型是 102–107，而它们要么注册即发
// （102）、要么走专用接口（103/104）、要么需用户自己动手（105–107），
// 全都进不了一键领取。所以「能被 activity/action 触发的」恰恰只剩未知类型，
// 这份 fixture 覆盖的就是这条兜底路径；已知类型的行为另由 ugTypeSpecs 用例锁。
const ugClaimableTableFixture = `{"commercial_activities":[
	{"activity_id":"promo_a","activity_type":999,"enabled":true,"work_extra":{"granted":false,"grantable":true,"general_credits":100}},
	{"activity_id":"promo_b","activity_type":999,"enabled":true,"work_extra":{"granted":false,"grantable":true,"general_credits":200}},
	{"activity_id":"promo_c","activity_type":999,"enabled":true,"work_extra":{"granted":false,"grantable":true,"work_credits":300}}
]}`

func TestClaimUGActivityActionNeedsPrereq(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(ugStub(map[string]string{
		EpUGActivityInfo:   ugClaimableTableFixture,
		EpUGActivityAction: `{}`,
	}, nil))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err != nil {
		t.Fatalf("ClaimUG 出错：%v", err)
	}
	result := ugResultByKey(t, results)["activity:promo_a"]
	if result.Status != UGNeedsPrereq {
		t.Fatalf("200 空对象应判为需先完成前置条件，得到 %v：%s", result.Status, result.Message)
	}
	if strings.TrimSpace(result.Message) == "" {
		t.Fatal("需前置的结果必须给出说明，不能是空文案")
	}
}

func TestClaimUGActivityInfoFailureKeepsFixedResults(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpUGActivityInfo {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":500,"message":"内部错误"}`))
			return
		}
		if r.URL.Path == EpCodeFissionShareStatus {
			// 这期没有裂变活动：结果里只该剩固定端点那两条。
			_, _ = w.Write([]byte(`{"code":9090,"message":"活动暂不可用"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"message":"ok"}`))
	}))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err != nil {
		t.Fatalf("活动表故障不该让整轮失败：%v", err)
	}
	// 固定端点照跑，活动表故障要留一条可见的失败——静默吞掉会让
	// 「活动中心挂了」和「这轮确实没活动」长得一模一样。
	if len(results) != len(UGActions())+1 {
		t.Fatalf("活动表故障时应是固定端点结果 + 一条失败，得到 %d 条：%+v", len(results), results)
	}
	byKey := ugResultByKey(t, results)
	failure := byKey["activities"]
	if failure.Status != UGFailed {
		t.Fatalf("活动表故障应记为 UGFailed，得到 %v", failure.Status)
	}
	if !strings.Contains(failure.Message, "500") {
		t.Fatalf("失败原因应带上上游状态码，得到 %q", failure.Message)
	}
}

func TestClaimUGFissionReportsInvitees(t *testing.T) {
	stubHost(t)
	log := &ugCallLog{}
	server := httptest.NewServer(ugStub(map[string]string{
		EpCodeFissionShareStatus: `{"code":0,"invitees":[{"invitee_name":"a","inviter_reward_credits":100,"status":2}]}`,
	}, log))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err != nil {
		t.Fatalf("ClaimUG 出错：%v", err)
	}
	result := ugResultByKey(t, results)["fission:"+fissionActivityCandidates[0]]
	if result.Status != UGAlready || !strings.Contains(result.Message, "累计 100 积分") {
		t.Fatalf("裂变进度不对：%v / %s", result.Status, result.Message)
	}
	if got := log.count(EpCodeFissionGrant); got != 0 {
		t.Fatalf("没有待发放的邀请时不该调 grant，实际 %d 次", got)
	}
}

func TestClaimUGFissionGrantsPendingInvitee(t *testing.T) {
	stubHost(t)
	log := &ugCallLog{}
	server := httptest.NewServer(ugStub(map[string]string{
		EpCodeFissionShareStatus: `{"code":0,"invitees":[{"invitee_name":"a","inviter_reward_credits":100,"status":1}]}`,
		EpCodeFissionGrant:       `{"code":0,"message":"发放成功"}`,
	}, log))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err != nil {
		t.Fatalf("ClaimUG 出错：%v", err)
	}
	byKey := ugResultByKey(t, results)
	if r := byKey["fission:"+fissionActivityCandidates[0]]; r.Status != UGUnavailable || !strings.Contains(r.Message, "待发放") {
		t.Fatalf("有待发放邀请时进度应如实说明：%v / %s", r.Status, r.Message)
	}
	if got := log.count(EpCodeFissionGrant); got != 1 {
		t.Fatalf("有待发放邀请时应调 grant 一次，实际 %d 次", got)
	}
	if r := byKey["fission-grant:"+fissionActivityCandidates[0]]; r.Status != UGClaimed {
		t.Fatalf("grant 成功应记为已领取：%+v", r)
	}
}

func TestClaimUGFissionShowsInviteCodeWhenNoInvitees(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(ugStub(map[string]string{
		EpCodeFissionShareStatus: `{"code":0,"invitees":[]}`,
		EpCodeFissionShareLink:   `{"code":0,"invite_code":"ABC123","share_url":"https://example.invalid/s/abc"}`,
	}, nil))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err != nil {
		t.Fatalf("ClaimUG 出错：%v", err)
	}
	result := ugResultByKey(t, results)["fission:"+fissionActivityCandidates[0]]
	if !strings.Contains(result.Message, "ABC123") {
		t.Fatalf("还没有被邀请人时应把邀请码带出来：%q", result.Message)
	}
}

func TestClaimUGFissionIgnoresReplyWithoutBusinessCode(t *testing.T) {
	stubHost(t)
	// 200 + `{"data":{}}`：网关兜页、被中间层改写都长这样。缺 code 不能当
	// 「这一期存在」，否则会把一张空壳报成活动进行中。
	server := httptest.NewServer(ugStub(map[string]string{
		EpCodeFissionShareStatus: `{"data":{}}`,
	}, nil))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err != nil {
		t.Fatalf("ClaimUG 出错：%v", err)
	}
	if _, ok := ugResultByKey(t, results)["fission:"+fissionActivityCandidates[0]]; ok {
		t.Fatal("没有业务码的响应不该被当成裂变期")
	}
}

func TestClaimUGFissionCredentialFailureStops(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCodeFissionShareStatus {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"message":"登录已过期"}`))
			return
		}
		if ugNeutralEndpoint(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"message":"ok"}`))
	}))
	defer server.Close()

	if _, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL)); err == nil {
		t.Fatal("凭证失效应让整轮报错，而不是静默跳过裂变")
	}
}

func TestSummarizeUGCountsPrereqAsPending(t *testing.T) {
	summary := SummarizeUG([]UGResult{
		{Key: "claim_package", Label: "每日 200 积分", Status: UGClaimed, Message: "领到 200 积分"},
		{Key: "activity:student_202610", Label: "学生认证 500 积分", Status: UGNeedsPrereq, Message: "需先完成学生认证"},
	})
	if !strings.Contains(summary, "待满足前置 1 项") {
		t.Fatalf("需前置的活动应单列计数：%s", summary)
	}
	if !strings.Contains(summary, "失败 0 项") {
		t.Fatalf("需前置不该计入失败：%s", summary)
	}
}

// TestSummarizeUGShowsInviteCode 验证：带信息量的「暂不可领」必须逐条展示。
//
// 邀请码、「已邀请 N 人其中 M 人待发放」、「活动未开始」都走 UGUnavailable +
// Message，压成名字列表等于把用户唯一能行动的信息丢掉。
func TestSummarizeUGShowsInviteCode(t *testing.T) {
	summary := SummarizeUG([]UGResult{
		{Key: "fission:1", Label: "分享裂变", Status: UGUnavailable, Message: "还没有被邀请人，邀请码 6DXPMKYC5V23"},
		{Key: "fission:2", Label: "分享裂变", Status: UGUnavailable, Message: "已邀请 3 人，其中 2 人待发放"},
		{Key: "activity:x", Label: "作品抽奖", Status: UGUnavailable, Message: "当前不可领"},
	})
	if !strings.Contains(summary, "6DXPMKYC5V23") {
		t.Fatalf("邀请码必须在汇总里可见，得到：%s", summary)
	}
	if !strings.Contains(summary, "2 人待发放") {
		t.Fatalf("待发放进度必须在汇总里可见，得到：%s", summary)
	}
	if !strings.Contains(summary, "作品抽奖") {
		t.Fatalf("无信息量的条目应压成名字列表，得到：%s", summary)
	}
}

// TestSummarizeUGKeepsSoftUnavailableCompact 验证：上游泛化文案仍然压缩。
func TestSummarizeUGKeepsSoftUnavailableCompact(t *testing.T) {
	summary := SummarizeUG([]UGResult{
		{Key: "a", Label: "甲", Status: UGUnavailable, Message: "活动暂不可用"},
		{Key: "b", Label: "乙", Status: UGUnavailable, Message: ""},
	})
	if strings.Count(summary, "\n") > 0 {
		t.Fatalf("泛化文案应压成一行，得到：%s", summary)
	}
	if !strings.Contains(summary, "甲") || !strings.Contains(summary, "乙") {
		t.Fatalf("压缩后仍要点名，得到：%s", summary)
	}
}

// TestClaimUGActivityAction401AbortsRound 验证：活动触发 401 与固定端点同口径。
//
// 继续打剩下每条活动只会拿同一个 401，宿主也看不到「需要重新登录」。
func TestClaimUGActivityAction401AbortsRound(t *testing.T) {
	stubHost(t)
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpUGActivityInfo:
			_, _ = w.Write([]byte(ugClaimableTableFixture))
		case EpUGActivityAction:
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"message":"unauthorized"}`))
		case EpCodeFissionShareStatus:
			_, _ = w.Write([]byte(`{"code":9090,"message":"活动暂不可用"}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"message":"ok"}`))
		}
	}))
	defer server.Close()

	_, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err == nil {
		t.Fatal("活动触发 401 应终止整轮并返回错误")
	}
	failure, ok := err.(*core.Failure)
	if !ok || failure.Kind != core.FailureKindCredential {
		t.Fatalf("401 应归为凭证失效，得到 %T %v", err, err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("凭证失效后不该继续打剩下活动，实际打了 %d 次", got)
	}
}

// TestClaimUGActivityActionSuccessMessageIsClaimed 验证：code=0 一律算已领取。
//
// "success" 是最常见的成功文案，把它当前置条件会把正常发放记成失败。
func TestClaimUGActivityActionSuccessMessageIsClaimed(t *testing.T) {
	for _, message := range []string{"success", "", "SUCCESS", "领取成功"} {
		t.Run(message, func(t *testing.T) {
			stubHost(t)
			server := httptest.NewServer(ugStub(map[string]string{
				EpUGActivityInfo:   ugClaimableTableFixture,
				EpUGActivityAction: `{"code":0,"message":"` + message + `"}`,
			}, nil))
			defer server.Close()

			results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
			if err != nil {
				t.Fatalf("不该报错：%v", err)
			}
			result := ugResultByKey(t, results)["activity:promo_a"]
			if result.Status != UGClaimed {
				t.Fatalf("code=0 message=%q 应记为已领取，得到 %v：%s", message, result.Status, result.Message)
			}
		})
	}
}

// TestClaimUGEnabledMissingStillClaimed 验证：enabled 缺失时按启用处理。
//
// 上游哪天不再下发这个字段，整张表被静默跳过是最难发现的一类失效。
func TestClaimUGEnabledMissingStillClaimed(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(ugStub(map[string]string{
		EpUGActivityInfo: `{"commercial_activities":[
			{"activity_id":"no_flag","activity_type":999,"work_extra":{"granted":false,"grantable":true,"general_credits":100}},
			{"activity_id":"off_flag","activity_type":999,"enabled":false,"work_extra":{"granted":false,"grantable":true,"general_credits":100}}
		]}`,
		EpUGActivityAction: `{"code":0,"message":"success"}`,
	}, nil))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	byKey := ugResultByKey(t, results)
	if _, ok := byKey["activity:no_flag"]; !ok {
		t.Fatalf("enabled 缺失应按启用处理并照领，实际结果：%+v", results)
	}
	if _, ok := byKey["activity:off_flag"]; ok {
		t.Fatalf("enabled=false 的活动不该被触发，实际结果：%+v", results)
	}
}

// TestClaimUGFissionStatusErrorVisible 验证：裂变探测失败要留一条可见失败。
func TestClaimUGFissionStatusErrorVisible(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpUGActivityInfo:
			_, _ = w.Write([]byte(`{"commercial_activities":[
				{"activity_id":"fis_1","activity_type":103,"enabled":true,"work_extra":{"granted":false}}
			]}`))
		case EpCodeFissionShareStatus:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":500,"message":"内部错误"}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"message":"ok"}`))
		}
	}))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err != nil {
		t.Fatalf("单期裂变查询失败不该让整轮失败：%v", err)
	}
	// 裂变期号来自 EpCodeFissionShareStatus 的固定参数，不是活动表里的 id。
	result, ok := ugResultByKey(t, results)["fission:code_fission_202609"]
	if !ok {
		t.Fatalf("裂变查询失败必须留一条结果，实际：%+v", results)
	}
	if result.Status != UGFailed {
		t.Fatalf("裂变查询失败应记为 UGFailed，得到 %v：%s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "500") {
		t.Fatalf("失败原因应带上上游状态码，得到 %q", result.Message)
	}
}

// TestClaimUGKeepsClaimedOn401 锁住「凭证失效时已经领到的不许丢」。
// 401 之前的结果是用户唯一能看到的账，直接 return results（不含 claimed）
// 会表现为「积分到账了，但报告里一条都没有」。
func TestClaimUGKeepsClaimedOn401(t *testing.T) {
	stubHost(t)
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpUGActivityInfo:
			_, _ = w.Write([]byte(ugClaimableTableFixture))
		case EpUGActivityAction:
			if atomic.AddInt32(&calls, 1) == 1 {
				_, _ = w.Write([]byte(`{"code":0,"message":"发放成功"}`))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"message":"unauthorized"}`))
		case EpCodeFissionShareStatus:
			_, _ = w.Write([]byte(`{"code":9090,"message":"活动暂不可用"}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"message":"ok"}`))
		}
	}))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err == nil {
		t.Fatal("活动触发 401 应返回错误")
	}
	claimed := 0
	for _, result := range results {
		if result.Status == UGClaimed && strings.HasPrefix(result.Key, "activity:") {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("401 之前领到的必须带回，实际 claimed=%d：%+v", claimed, results)
	}
}

// TestUGRoundBudgetScalesWithFissionCandidates 锁住预算随裂变候选数伸缩。
// 写死 45s 的话，fissionActivityCandidates 加一期就会从后面几期嘴里抢时间。
func TestUGRoundBudgetScalesWithFissionCandidates(t *testing.T) {
	if len(fissionActivityCandidates) == 0 {
		t.Fatal("裂变候选不能为空，否则 reserve 恒为 0")
	}
	want := time.Duration(len(fissionActivityCandidates)) * 3 * ugDynamicTimeout
	if ugFissionReserve() != want {
		t.Fatalf("ugFissionReserve() = %s，want %s（候选数 × 3 次 × ugDynamicTimeout）", ugFissionReserve(), want)
	}
	worst := 2*perRequestTimeout + 3*ugDynamicTimeout + ugFissionReserve()
	if ugRoundTimeout() < worst {
		t.Fatalf("ugRoundTimeout() = %s 小于最坏耗时 %s", ugRoundTimeout(), worst)
	}
	if ugRoundTimeout() > 3*time.Minute {
		t.Fatalf("ugRoundTimeout() = %s 超过宿主 requestTimeout(3min)，会被外层截断", ugRoundTimeout())
	}

	// 追加一期后预算与总上限必须立刻跟着涨——这正是改成函数的原因：
	// 包级 var 只在初始化时算一次，之后 append 到候选表不会反映进来。
	original := fissionActivityCandidates
	fissionActivityCandidates = append(original[:len(original):len(original)], original[0])
	defer func() { fissionActivityCandidates = original }()
	if got := ugFissionReserve(); got != want+3*ugDynamicTimeout {
		t.Fatalf("追加候选后 ugFissionReserve() = %s，want %s", got, want+3*ugDynamicTimeout)
	}
	if got := ugRoundTimeout(); got != worst+4*ugDynamicTimeout {
		t.Fatalf("追加候选后 ugRoundTimeout() = %s，want %s", got, worst+4*ugDynamicTimeout)
	}
}

// TestUGAlreadyDoesNotRepeatItself 锁住「已领取」的说明只在带新信息时才拼。
// 固定端点的 message 本身就是「该奖励已领取」，活动总览给的是「已发放」，
// 再拼一遍会变成「此前已领取（该奖励已领取）」；裂变的邀请进度必须保留。
func TestUGAlreadyDoesNotRepeatItself(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    string
	}{
		{"固定端点重复文案", "该奖励已领取", "每日签到：此前已领取"},
		{"总览只说已发放", "已发放", "每日签到：此前已领取"},
		{"裂变进度必须保留", "已邀请 3 人，累计 300 积分", "每日签到：此前已领取（已邀请 3 人，累计 300 积分）"},
		{"裂变带已发放进度不被抑制", "已邀请 5 人，已发放 3 人", "每日签到：此前已领取（已邀请 5 人，已发放 3 人）"},
		{"没有文案", "", "每日签到：此前已领取"},
	}
	for _, c := range cases {
		got := UGResult{Label: "每日签到", Status: UGAlready, Message: c.message}.Describe()
		if got != c.want {
			t.Fatalf("%s: Describe() = %q，want %q", c.name, got, c.want)
		}
	}
}

// TestUGEndpointTableIsConsistent 锁住动作表的两个不变量：
//   - 需要入参的端点不进批量入口（否则每轮白打注定 4001 的请求）；
//   - 每个端点都有 key/label/path 与调用函数，key 不重复。
func TestUGEndpointTableIsConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, endpoint := range ugEndpointTable {
		if endpoint.Key == "" || endpoint.Label == "" || endpoint.Path == "" || endpoint.Run == nil {
			t.Fatalf("%q: key/label/path/run 都不能为空", endpoint.Key)
		}
		if seen[endpoint.Key] {
			t.Fatalf("动作 key 重复: %s", endpoint.Key)
		}
		seen[endpoint.Key] = true
	}
	for _, action := range UGActions() {
		if endpoint, okEndpoint := ugEndpointByKey(action.Key); !okEndpoint || endpoint.NeedsInput {
			t.Fatalf("批量入口里出现了需要入参的端点: %s", action.Key)
		}
	}
	for _, endpoint := range ugEndpointTable {
		if !endpoint.NeedsInput {
			continue
		}
		for _, action := range UGActions() {
			if action.Key == endpoint.Key {
				t.Fatalf("需要入参的端点不该进批量入口: %s", endpoint.Key)
			}
		}
	}
}

// TestRunUGNeedsInputSkipsRequest 锁住「缺入参的动作不发请求」：
// 直接返回暂不可领，而不是打一次注定失败的请求再把它包装成故障。
func TestRunUGNeedsInputSkipsRequest(t *testing.T) {
	endpoint, okEndpoint := ugEndpointByKey("cobrand_claim")
	if !okEndpoint {
		t.Fatal("动作表里应登记 cobrand_claim")
	}
	if !endpoint.NeedsInput {
		t.Fatal("cobrand_claim 需要活动 ID，应标记 needs_input")
	}
	// 指向不可达主机：一旦真发请求就会得到 UGFailed。
	result, errRun := endpoint.Run(context.Background(), ugTestConfig(t, "http://127.0.0.1:1"), nil, endpoint)
	if errRun != nil {
		t.Fatalf("缺入参不应返回错误: %v", errRun)
	}
	if result.Status != UGUnavailable {
		t.Fatalf("status = %v, 期望 UGUnavailable", result.Status)
	}
}

// TestUGAutoStatusThreeStates 锁住一键任务的三态映射：
// 领到 = done、只有失败 = error、已领/暂不可领 = skipped。
//
// 把「无事可做」算成 done 会让用户以为今天领到了东西；把「跳过」算成 error
// 会让正常账号在日志里天天刷红。
func TestUGAutoStatusThreeStates(t *testing.T) {
	cases := []struct {
		name    string
		results []UGResult
		want    string
	}{
		{"没有入口", nil, core.AutoStatusSkipped},
		{"领到一项", []UGResult{{Status: UGClaimed}, {Status: UGUnavailable}}, core.AutoStatusDone},
		{"全部失败", []UGResult{{Status: UGFailed}, {Status: UGFailed}}, core.AutoStatusError},
		{"已领过", []UGResult{{Status: UGAlready}, {Status: UGUnavailable}}, core.AutoStatusSkipped},
		{"部分失败但有已领", []UGResult{{Status: UGFailed}, {Status: UGAlready}}, core.AutoStatusSkipped},
		{"待前置", []UGResult{{Status: UGNeedsPrereq}}, core.AutoStatusSkipped},
	}
	for _, testCase := range cases {
		if got, _ := ugAutoStatus(testCase.results); got != testCase.want {
			t.Fatalf("%s: ugAutoStatus = %q, want %q", testCase.name, got, testCase.want)
		}
	}
}
