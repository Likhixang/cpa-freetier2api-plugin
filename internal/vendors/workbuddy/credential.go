package workbuddy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/internal/core"
)

// 本文件实现凭证的解析与写回。
//
// 凭证有两种形态（历史原因，两种都要认）：
//   - 嵌套形：{"auth": {...}, "account": {...}, "device_token": ...}（OAuth 登录产物）
//   - 扁平形：顶层直接放 accessToken / refreshToken / uid / ...（手写或旧版）
//
// **并发约定**：所有出站请求头构造必须经取值方法（AccessTokenValue 等）加锁快照，
// 不能直读字段——任务调度器与请求转发会真并发地刷新/读取同一份凭证。

// protocolName 是本协议在文件名里的公共标识段。
//
// CN 与 GLOBAL 的凭证文件名分别是 workbuddycn-<uid>.json 与
// workbuddyglobal-<uid>.json，两者都含 "workbuddy" 段，因此用它做协议级
// 的文件名识别；具体区域由 core.ResolveVendor 的完整前缀匹配决定。
const protocolName = "workbuddy"

// Credential 是归一化后的账号凭证。
type Credential struct {
	mu sync.Mutex

	// AccessToken 是出站 Authorization 的 Bearer 值。
	AccessToken string
	// RefreshToken 用于刷新 access token。
	RefreshToken string
	// ExpiresAt 是 access token 过期时刻（Unix 秒）。
	ExpiresAt int64
	// Domain 是账号所属域名（用于推断 realm 与出站 X-Domain 头）。
	Domain string
	// realm 未导出：显式声明优先，为空时按 Domain 推断。
	realm Region
	// UID 是账号唯一标识（也是设备指纹的派生种子）。
	UID string
	// EnterpriseID 是企业租户标识（CN 的 X-Enterprise-Id）。
	EnterpriseID string
	// Nickname 是展示用昵称。
	Nickname string
	// DeviceToken 是 X-Device-Token；空表示不注入。
	DeviceToken string
	// FilePath 是凭证来源文件路径（刷新后原子写回此处）；空表示非文件来源。
	FilePath string
}

// Credential 的顶层 JSON 形态。
type credentialJSON struct {
	Auth *struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
		Realm        string `json:"realm"`
	} `json:"auth"`
	Account *struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	} `json:"account"`
	DeviceToken string `json:"device_token"`

	// 扁平形态的字段。
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// RegisterPathHint 是凭证文件名的识别提示（auth.parse 用它判断文件归属）。
const RegisterPathHint = "workbuddy"

// ParseCredential 解析凭证 JSON。
//
// fallbackRegion 是账号未声明 realm 时的兜底域（来自插件配置）。
func ParseCredential(raw []byte, fallbackRegion Region) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("credential is empty")
	}
	var parsed credentialJSON
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal != nil {
		return nil, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}

	cred := &Credential{}
	if parsed.Auth != nil {
		cred.AccessToken = strings.TrimSpace(parsed.Auth.AccessToken)
		cred.RefreshToken = strings.TrimSpace(parsed.Auth.RefreshToken)
		cred.ExpiresAt = parsed.Auth.ExpiresAt
		cred.Domain = strings.TrimSpace(parsed.Auth.Domain)
		cred.realm = normalizeRealmStrict(parsed.Auth.Realm)
	} else {
		cred.AccessToken = strings.TrimSpace(parsed.AccessToken)
		cred.RefreshToken = strings.TrimSpace(parsed.RefreshToken)
		cred.ExpiresAt = parsed.ExpiresAt
		cred.Domain = strings.TrimSpace(parsed.Domain)
		cred.realm = normalizeRealmStrict(parsed.Realm)
	}
	if parsed.Account != nil {
		cred.UID = strings.TrimSpace(parsed.Account.UID)
		cred.EnterpriseID = strings.TrimSpace(parsed.Account.EnterpriseID)
		cred.Nickname = strings.TrimSpace(parsed.Account.Nickname)
	} else {
		cred.UID = strings.TrimSpace(parsed.UID)
		cred.EnterpriseID = strings.TrimSpace(parsed.EnterpriseID)
		cred.Nickname = strings.TrimSpace(parsed.Nickname)
	}
	cred.DeviceToken = strings.TrimSpace(parsed.DeviceToken)

	if cred.AccessToken == "" && cred.RefreshToken == "" {
		return nil, fmt.Errorf("credential has neither accessToken nor refreshToken")
	}
	if cred.realm == "" {
		if inferred := realmFromDomain(cred.Domain); inferred != "" {
			cred.realm = inferred
		} else {
			cred.realm = fallbackRegion
		}
	}
	return cred, nil
}

// normalizeRealmStrict 只接受明确的 cn / global 值，其余返回空串。
func normalizeRealmStrict(raw string) Region {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(RegionCN):
		return RegionCN
	case string(RegionGlobal):
		return RegionGlobal
	}
	return ""
}

// realmFromDomain 按域名推断域。
//
// 用域名后缀而非完全匹配：国际版有多个子域（www.workbuddy.ai、api...），
// 而 cn 域全部落在 codebuddy.cn / tencent.com 之下。
func realmFromDomain(domain string) Region {
	trimmed := strings.ToLower(strings.TrimSpace(domain))
	trimmed = strings.TrimPrefix(trimmed, "https://")
	trimmed = strings.TrimPrefix(trimmed, "http://")
	trimmed = strings.TrimSuffix(trimmed, "/")
	if trimmed == "" {
		return ""
	}
	if trimmed == "workbuddy.ai" || strings.HasSuffix(trimmed, ".workbuddy.ai") {
		return RegionGlobal
	}
	return RegionCN
}

// Realm 返回凭证所属域。
func (c *Credential) Realm() Region {
	if c == nil {
		return RegionCN
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.realm
}

// SetRealm 覆盖凭证域（账号级覆盖优先于插件配置兜底）。
func (c *Credential) SetRealm(region Region) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.realm = region
}

// AccessTokenValue 加锁读取 access token。
func (c *Credential) AccessTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken
}

// RefreshTokenValue 加锁读取 refresh token。
func (c *Credential) RefreshTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.RefreshToken
}

// DomainValue 加锁读取域名。
func (c *Credential) DomainValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Domain
}

// UIDValue 加锁读取 UID。
func (c *Credential) UIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.UID
}

// NicknameValue 加锁读取昵称。
func (c *Credential) NicknameValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Nickname
}

// EnterpriseIDValue 加锁读取企业标识。
func (c *Credential) EnterpriseIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.EnterpriseID
}

// DeviceTokenValue 加锁读取设备令牌。
func (c *Credential) DeviceTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.DeviceToken
}

// ApplyTokenRefresh 写回刷新结果。
//
// 采用「两段式快照校验」：调用方在发起网络请求前取快照，拿到响应后在本方法里
// 比对当前值是否仍是快照——若已被并发刷新过，则放弃写回，避免用旧响应覆盖新值。
func (c *Credential) ApplyTokenRefresh(accessToken, refreshToken, domain string, expiresAt int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if trimmed := strings.TrimSpace(accessToken); trimmed != "" {
		c.AccessToken = trimmed
	}
	if trimmed := strings.TrimSpace(refreshToken); trimmed != "" {
		c.RefreshToken = trimmed
	}
	if trimmed := strings.TrimSpace(domain); trimmed != "" {
		c.Domain = trimmed
	}
	if expiresAt > 0 {
		c.ExpiresAt = expiresAt
	}
}

// Snapshot 返回 access/refresh token 的当前值，供两段式刷新校验使用。
func (c *Credential) Snapshot() (accessToken, refreshToken string) {
	if c == nil {
		return "", ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken, c.RefreshToken
}

// UnchangedSince 报告凭证是否仍与快照一致。
func (c *Credential) UnchangedSince(accessToken, refreshToken string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken == accessToken && c.RefreshToken == refreshToken
}

// StorageJSON 把凭证序列化为嵌套形态，用于写回 CPA 的凭证文件。
//
// 写成嵌套形是刻意的：CPA 的 auth 文件里还可能有用户自己维护的字段
// （disabled / prefix / proxy_url / note / weight），调用方应先做**合并更新**
// 而不是用本方法的输出整体替换（见 MergeStorageJSON）。
func (c *Credential) StorageJSON() ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("credential is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	auth := map[string]any{
		"accessToken":  c.AccessToken,
		"refreshToken": c.RefreshToken,
		"expiresAt":    c.ExpiresAt,
	}
	if c.Domain != "" {
		auth["domain"] = c.Domain
	}
	if c.realm != "" {
		auth["realm"] = string(c.realm)
	}
	account := map[string]any{}
	if c.UID != "" {
		account["uid"] = c.UID
	}
	if c.EnterpriseID != "" {
		account["enterpriseId"] = c.EnterpriseID
	}
	if c.Nickname != "" {
		account["nickname"] = c.Nickname
	}
	out := map[string]any{
		// type 必须写入：宿主用它**直接判定归属**，有它就按 provider 找到
		// 本插件，不再遍历插件。缺它时宿主会把「当前询问的插件的 identifier」
		// 兜底填进 provider，归属判定过宽的前置插件会抢走本插件的凭证，
		// 导致账号在宿主侧被归到别的 provider（页面表现为账号凭空消失）。
		"type": ProviderKey,
		"auth": auth,
	}
	if len(account) > 0 {
		out["account"] = account
	}
	if c.DeviceToken != "" {
		out["device_token"] = c.DeviceToken
	}
	return json.MarshalIndent(out, "", "  ")
}

// MergeStorageJSON 在原凭证 JSON 上做合并更新，返回新的字节。
//
// 基于原文件合并而不是从头构造：CPA 的 auth 文件里可能有用户自己维护的
// disabled / prefix / proxy_url / note / weight 等字段，重建会静默丢掉它们。
func MergeStorageJSON(original []byte, cred *Credential) ([]byte, error) {
	updated, errCred := cred.StorageJSON()
	if errCred != nil {
		return original, errCred
	}
	if len(original) == 0 {
		return updated, nil
	}
	var existing map[string]any
	if errUnmarshal := json.Unmarshal(original, &existing); errUnmarshal != nil {
		// 原文件不是对象（或已损坏）：退回整体替换。
		return updated, nil
	}
	var fresh map[string]any
	if errUnmarshal := json.Unmarshal(updated, &fresh); errUnmarshal != nil {
		return original, errUnmarshal
	}
	mergeMaps(existing, fresh)
	// 兜底：无论原文件有没有 type，合并结果都要带它。
	// 用户从别处复制进来的旧凭证常常缺这个字段，补上后归属就确定了。
	if _, okType := existing["type"]; !okType {
		existing["type"] = ProviderKey
	}
	return json.MarshalIndent(existing, "", "  ")
}

// mergeMaps 把 src 的键值递归合并进 dst（src 优先）。
func mergeMaps(dst, src map[string]any) {
	for key, value := range src {
		if srcMap, okSrc := value.(map[string]any); okSrc {
			if dstMap, okDst := dst[key].(map[string]any); okDst {
				mergeMaps(dstMap, srcMap)
				continue
			}
		}
		dst[key] = value
	}
}

// SaveCredentialFile 原子写回凭证文件。
func SaveCredentialFile(path string, cred *Credential) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil
	}
	original, errRead := os.ReadFile(trimmed)
	if errRead != nil {
		original = nil
	}
	merged, errMerge := MergeStorageJSON(original, cred)
	if errMerge != nil {
		return fmt.Errorf("merge credential file %s: %w", trimmed, errMerge)
	}
	tmp := trimmed + ".tmp"
	if errWrite := os.WriteFile(tmp, merged, 0o600); errWrite != nil {
		return fmt.Errorf("write credential file %s: %w", tmp, errWrite)
	}
	if errRename := os.Rename(tmp, trimmed); errRename != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace credential file %s: %w", trimmed, errRename)
	}
	return nil
}

// LooksLikeCredential 判断一份 JSON 是否属于本插件（任意区域）。
//
// **归属判定必须偏严**：宿主会把所有非内建格式的凭证文件依次喂给每个插件，
// 一旦误吞别家的凭证，宿主会用它写出本插件的凭证结构，等于污染了对方的账号
// （而且这个错误是静默的）。反过来，漏认只是加载失败，用户立刻会发现。
//
// 因此只接受**本插件特有的证据**，从强到弱：
//  1. 文件里的 type 字段或宿主传入的 provider 提示是 freetier；
//  2. 文件名符合本插件的命名约定（workbuddycn / workbuddyglobal 前缀/中缀）；
//  3. 具备本插件独有的嵌套结构（auth.accessToken + auth.refreshToken）。
//
// 刻意**不**接受顶层的 accessToken / refreshToken / device_token：
// 这些是通用字段，别的 provider 也用（例如某些插件同样写 device_token），
// 拿它们做判据必然跨插件误吞。
//
// 注意：本函数只判断「属于本协议」，**不判断区域**——区域由 core.ResolveVendor
// 的文件名前缀与 vendor 字段决定，或由具体 vendor 实例的 Match 二次确认。
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	// 1) 显式声明（最可靠，也是推荐的写法：支持当前 freetier 及历史 workbuddy / codebuddy 标识）。
	for _, expected := range []string{ProviderKey, "workbuddy", "codebuddy"} {
		if strings.EqualFold(strings.TrimSpace(provider), expected) {
			return true
		}
		if typeValue, okType := raw["type"].(string); okType && strings.EqualFold(strings.TrimSpace(typeValue), expected) {
			return true
		}
	}

	// 2) 文件名约定。
	if core.FileNameMatchesConvention(fileName, VendorIDCN, VendorIDGlobal, protocolName) {
		return true
	}

	// 3) 本插件独有的嵌套结构：auth 里同时有 accessToken 与 refreshToken。
	// 单有 accessToken 不足以判定（其它 provider 也可能这样嵌套）。
	if nested, okNested := raw["auth"].(map[string]any); okNested {
		_, hasAccess := nested["accessToken"]
		_, hasRefresh := nested["refreshToken"]
		if hasAccess && hasRefresh {
			return true
		}
	}

	// 4) 域名特征：workbuddy.ai 或 codebuddy.cn 域名。
	domain := core.FirstNonEmpty(core.GetString(raw, "domain"), core.GetString(core.GetMap(raw, "auth"), "domain"))
	if d := strings.ToLower(domain); strings.Contains(d, "workbuddy.ai") || strings.Contains(d, "codebuddy.cn") {
		return true
	}
	return false
}

// ProviderKey 是本插件在 CPA 里占用的 provider 键。
//
// **所有供应商共用它**：宿主的 auth.identifier 只返回单个字符串、注册期调一次
// 永久缓存，且与 auth 文件的 type 字段严格相等才认领该文件。因此供应商的区分
// 落在凭证文件名前缀（workbuddycn-<uid>.json）与文件内的 vendor 字段上，
// 而不是 provider key。
const ProviderKey = core.ProviderKey

// RegionForCredential 从凭证 JSON 推断所属区域，推断不出返回空。
//
// 判定顺序：嵌套形的 auth.realm → auth.domain 后缀 → 扁平形的 realm → domain 后缀。
// 导出给根层的 vendor 适配器用：凭证归属判定需要它，而判定逻辑属于协议层。
func RegionForCredential(raw map[string]any) Region {
	if raw == nil {
		return ""
	}
	if nested, okNested := raw["auth"].(map[string]any); okNested {
		if realm := realmField(nested); realm != "" {
			return realm
		}
	}
	return realmField(raw)
}

// realmField 从一层 JSON 里读区域：先看显式 realm 字段，再看 domain 后缀。
func realmField(level map[string]any) Region {
	if realm, okRealm := level["realm"].(string); okRealm && strings.TrimSpace(realm) != "" {
		if strict := normalizeRealmStrict(realm); strict != "" {
			return strict
		}
	}
	if domain, okDomain := level["domain"].(string); okDomain {
		if inferred := realmFromDomain(domain); inferred != "" {
			return inferred
		}
	}
	return ""
}

// 本文件实现 WorkBuddy（国内版）的每日签到。
//
// daily-checkin 端点只有国内版入账；国际版的每日收入路径是每日活跃奖励
// （活跃信标，见 tasks.go），由 Checkin/RunScheduled 按 realm 分流，
// 不应把国际版凭证打进这里的端点。

// 本文件实现 WorkBuddy 的凭证续期。
//
// 续期链路：POST <ChatBase>/v2/plugin/auth/token，用 X-Refresh-Token 头
// 带旧 refresh token 换一组新令牌。
//
// 安全红线：X-Refresh-Token 头**只允许出现在这个刷新端点**，对话请求
// 绝不能携带——把 refresh token 发给对话上游等于把长期凭据暴露给一个
// 只需要 access token 的服务。

// 本文件实现 WorkBuddy 的额度查询。
//
// 额度口径是**积分余额**：上游的计费接口按资源包返回 remain/used/size，
// 这里聚合成账号级的「剩余 / 总额」并保留套餐明细。

// packageEndLayout 是套餐到期时间的墙钟格式（UTC+8）。
const packageEndLayout = "2006-01-02 15:04:05"

// 本文件实现 CodeBuddy 的成长任务事件链与客户端指纹派生。
//
// 本包是纯逻辑层，依赖 cb 包完成出站（经宿主 HTTP 桥）。
//
// 整个体系的本质是**四套客户端指纹**：同一个 /v2/report 端点，
// 服务端按请求的指纹归属到不同的任务族：
//
//	CLI/billing 指纹  计费域 /v2/report    对话活跃上报（chat_5、连登、领养前置）
//	桌面指纹          对话域 /v2/report    桌面行为事件链（RichMeow、模板、专家…）
//	web 指纹          Web 域 /v2/report    资料库阅读等 web 侧行为
//	小程序指纹        计费域 /v2/report    开学季与小程序专属任务
//
// 三条普适规律（都是从真实失败中总结的）：
//   - /v2/report 的 body 恒为**数组**，不是单个对象；
//   - 上报 200 ≠ 计分，必须轮询回读（普通 4×3s、mp 2×3s、开学季 3×2.5s）；
//   - 事件必须带 userId，缺失时服务端 200 但静默丢弃。

// 节流参数。全部照抄原项目实测值——这些数字是风控边界，
// 调快会触发上游限流，调慢只是浪费时间。
const (
	// reportGap 是连续上报之间的间隔。
	reportGap = 1050 * time.Millisecond
	// mpActionGap 是小程序任务写动作之间的间隔。
	mpActionGap = 2 * time.Second
	// nightChatGap 是夜间对话之间的间隔。
	nightChatGap = 4 * time.Second
)

// 异步计分的轮询参数。
var (
	// schoolPollLoops / schoolPollGap 是开学季的等待预算（约 7.5 秒）。
	schoolPollLoops = 3
	schoolPollGap   = 2500 * time.Millisecond
)

// fingerprint 生成某个账号某用途的稳定设备标识。
//
// 36 位 hex（18 字节），与官方客户端的设备 id 长度一致。
// 跨重启稳定、账号间互异、部署间隔离（混入机器盐，见 runtime.configure）。
func randomHex(n int) string {
	return core.RandomHex(n)
}

// nowUnixMs 返回当前毫秒时间戳。
func nowUnixMs() int64 { return time.Now().UnixMilli() }

// desktopFingerprint 是桌面指纹族注入每个事件的公共字段。
//
// 这些字段共同把请求标识成「来自一台固定的 Windows 桌面客户端」。
// 业务字段可以覆盖它们（map 合并时业务优先）。
func desktopFingerprint(credential *Credential, rt *runtime) map[string]any {
	now := nowUnixMs()
	return map[string]any{
		"timezone":     "Asia/Shanghai",
		"reportDelay":  2000,
		"userId":       credential.UIDValue(),
		"username":     credential.NicknameValue(),
		"userNickname": credential.NicknameValue(),
		"product":      "SaaS",
		// releaseDate 与 commit 是官方桌面版的构建信息，固定值即可。
		"releaseDate": int64(1789036585355),
		"commit":      "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName":     "WorkBuddy",
		"ideType":     "WorkBuddy",
		"ideVersion":  DesktopClientVersion,
		"machineId":   rt.fingerprint(credential, "machine"),
		"sessionId":   rt.fingerprint(credential, "session"),
		"extName":     "workbuddy-desktop",
		"extVersion":  DesktopClientVersion,
		"os":          "win32",
		"arch":        "x64",
		"osVersion":   "10.0.26220",
		"cpuCores":    20,
		"memorySize":  24,
		"timestamp":   now,
		"presentAt":   now,
	}
}

// mergeEvent 把业务字段合并进指纹基底（业务字段优先）。
func mergeEvent(base map[string]any, event map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(event))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range event {
		merged[key] = value
	}
	return merged
}

// DesktopChatSequence 构造 6 事件对话链（多数任务的骨架）。
//
// 其中 chat_message_response 必须带 isSuccessful:true —— 服务端对事件链有
// 真实性校验，没有成功回执的链不会被计分。
func DesktopChatSequence(conversationID, requestID, messageID, modelID, modelName string) []map[string]any {
	now := nowUnixMs()
	traceID := requestID
	assistantMessageID := messageID + "-assistant"
	return []map[string]any{
		{
			"eventCode": "agent_task_created",
			"source":    "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
			"requestModelId": modelID, "requestModelName": modelName,
			"has_repo": false, "repo_type": "none", "workspace_type": "empty",
			"has_connector": false, "connector_types": []any{},
			"has_mention": false, "mention_types": []any{},
			"has_template": false, "action": "", "template_name": "",
			"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
			"has_skill": false, "skill_names": []any{},
			"conversationId": conversationID, "messageId": messageID,
			"buddyId": "", "buddyName": "",
		},
		{
			"eventCode": "chat_message_send",
			"messageId": assistantMessageID, "historyCount": 0, "isContextTruncated": false,
			"currentStepCount": 1, "traceId": traceID, "rootRequestId": requestID,
			"parentConversationId": conversationID, "agentName": "cli", "agentType": "main",
		},
		{
			"eventCode":   "chat_request_send",
			"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
			"isAutoModify": false, "codebaseEnable": false,
			"maxToken": 0, "maxSteps": 500, "temperature": 0, "maxRetries": 0,
			"mentionContexts": []any{}, "knowledgeId": knowledgeIDEmpty, "knowledgeName": knowledgeNameEmpty,
			"codebaseId": "", "mentionContextCount": 0, "command": "",
			"expertId": "", "recommendId": "", "skillId": "", "skillCount": 0,
			"totalCount": 0, "fileUri": "", "presentAt": now,
			"traceId": traceID, "rootRequestId": requestID, "parentConversationId": conversationID,
			"conversationId": conversationID, "messageId": messageID,
			"agentName": "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		},
		{
			"eventCode": "chat_message_response",
			"messageId": assistantMessageID, "responseModelId": modelID, "responseModelName": modelName,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"firstTokenAt": now, "traceId": traceID, "conversationId": conversationID,
			"parentConversationId": conversationID, "presentAt": now,
		},
		{
			"eventCode": "chat_message_status",
			"messageId": assistantMessageID, "messageErrorCode": "0",
			"traceId": traceID, "conversationId": conversationID, "presentAt": now,
		},
		{
			"eventCode": "chat_request_response",
			"mode":      "craft", "toolCallCount": 0,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"isSuccessful": true, "finishReason": "stop",
			"conversationId": conversationID, "requestId": requestID,
			"traceId": traceID, "parentConversationId": conversationID, "presentAt": now,
		},
	}
}

// DesktopBuddyAppSequence 构造「进入 Buddy 应用」事件链（5 事件）。
//
// 同一组事件同时满足 Buddy_App 与 Buddy_App_QQ 两个任务：
// 后者只要求「进入企鹅教师助手」，而本链的事件里带了该应用的标识。
func DesktopBuddyAppSequence(buddyID, buddyName string) []map[string]any {
	base := map[string]any{"mode": "LOCAL", "buddyId": buddyID, "buddyName": buddyName, "position": 2}
	return []map[string]any{
		mergeEvent(base, map[string]any{"eventCode": "buddyapp_discover_click"}),
		mergeEvent(base, map[string]any{"eventCode": "buddyapp_show", "elementId": buddyID, "elementName": buddyName}),
		mergeEvent(base, map[string]any{"eventCode": "buddyapp_enter_click", "elementId": buddyID, "elementName": buddyName, "isFirstPage": "1"}),
		mergeEvent(base, map[string]any{"eventCode": "buddyapp_auth_confirm_click"}),
		mergeEvent(base, map[string]any{"eventCode": "buddyapp_bindaccount_skip_click"}),
	}
}

// DefaultBuddyID / DefaultBuddyName 是企鹅教师助手应用（Buddy_App_QQ 要求的目标）。
const (
	DefaultBuddyID   = "cb_y5Dy46tPQGGWtueMxXbe"
	DefaultBuddyName = "企鹅教师助手"
)

// DesktopAutomationCreateEvent 构造「定时任务创建成功」事件（automation_1）。
func DesktopAutomationCreateEvent(name string) []map[string]any {
	return []map[string]any{{
		"eventCode": "automated_task_create_suc", "name": name, "source": "manually",
		"modelId": "fast-model", "modelIsThinking": true, "connectorCount": 0,
		"skills": "", "skillCount": 0, "scheduleType": "once", "mode": "LOCAL",
	}}
}

// DesktopTemplateUseSequence 构造模板使用链（template_5）。
//
// = 对话链 + 两个模板事件。template_id 服务端不校验真实性，因此可用占位 id。
func DesktopTemplateUseSequence(conversationID, requestID, templateID, templateName string) []map[string]any {
	events := DesktopChatSequence(conversationID, requestID, randomHex(16), "fast-model", "fast-model")
	events = append(events,
		map[string]any{
			"eventCode": "agent_task_created_with_template", "mode": "working",
			"isCustomModel": false, "id": templateID, "name": templateName, "requestId": requestID,
		},
		map[string]any{
			"eventCode": "template_used", "template_id": templateID, "task_mode": "working",
		},
	)
	return events
}

// DesktopPlaybookPromptSequence 构造灵感案例链（playbook_prompt）。
//
// 判据是 playbook_prompt_send（真正的发送动作），不是卡片点击或曝光。
func DesktopPlaybookPromptSequence(conversationID, requestID, caseID, caseName string) []map[string]any {
	payload := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
	}
	events := DesktopChatSequence(conversationID, requestID, randomHex(16), "fast-model", "fast-model")
	events = append(events,
		mergeEvent(payload, map[string]any{
			"eventCode": "web_element_click", "pageName": "playbook_detail",
			"elementId": "playbook_ctaClick", "elementName": caseName, "source": "discover",
		}),
		mergeEvent(payload, map[string]any{
			"eventCode": "playbook_cta_click", "source": "discover", "position": 0,
		}),
		mergeEvent(payload, map[string]any{
			"eventCode": "playbook_prompt_send", "conversationId": conversationID, "requestId": requestID,
		}),
	)
	return events
}

// DesktopDesignCanvasSequence 构造设计画布链（create_canvas）。
func DesktopDesignCanvasSequence(conversationID, requestID string) []map[string]any {
	events := DesktopChatSequence(conversationID, requestID, randomHex(16), "fast-model", "fast-model")
	suffix := requestID
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	events = append(events,
		map[string]any{
			"eventCode": "wbx_design_canvas_task_create", "conversationId": conversationID,
			"requestId": requestID, "source": "summon_keyword", "cost": 12000, "isSuccessful": true,
		},
		map[string]any{
			"eventCode": "wbx_design_canvas_open", "conversationId": conversationID,
			"requestId": requestID, "id": "ardot-file-" + suffix,
			"source": "summon_keyword", "type": "page", "cost": 13000, "isSuccessful": true,
		},
	)
	return events
}

// SchoolChatTimesEvents 构造小程序对话事件（开学季与 mp 任务共用）。
//
// activityID 非空时带上（开学季任务要求它，缺失则不点亮）。
func SchoolChatTimesEvents(conversationID, activityID string) []map[string]any {
	requestID := "wb2api-" + randomHex(8)
	messageID := "msg-" + requestID
	if len(requestID) > 8 {
		messageID = "msg-" + requestID[len(requestID)-8:]
	}
	event := map[string]any{
		"eventCode":   "chat_request_send",
		"inputLength": 14, "isPlan": false, "isAutoExecuteTerminal": false,
		"isAutoModify": false, "codebaseEnable": false,
		"maxToken": 0, "maxSteps": 500, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": knowledgeIDEmpty, "knowledgeName": knowledgeNameEmpty,
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"expertId": "", "recommendId": "", "skillId": "", "skillCount": 0,
		"totalCount": 0, "fileUri": "", "presentAt": nowUnixMs(),
		"traceId": requestID, "rootRequestId": requestID,
		"parentConversationId": conversationID, "conversationId": conversationID,
		"messageId": messageID, "agentName": "mp", "agentType": "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": requestID,
	}
	if activityID != "" {
		event["activityId"] = activityID
	}
	return []map[string]any{event}
}

// MiniChatModelEvent 构造带指定模型的小程序对话事件。
func MiniChatModelEvent(conversationID, modelID, modelName string) []map[string]any {
	events := SchoolChatTimesEvents(conversationID, "")
	if len(events) == 0 {
		return events
	}
	events[0]["requestModelId"] = modelID
	events[0]["requestModelName"] = modelName
	return events
}

// MiniExpertUseEvent 构造小程序专家使用事件（Sequential_Tasks_2）。
//
// 注意：**不带 conversationId、不带 activityId** —— 真实事件就是这两个字段都没有。
// expertID 必须是市场里的真实 ex_ id，空 id 服务端不入账。
func MiniExpertUseEvent(expertID, expertName, expertType string) []map[string]any {
	return []map[string]any{{
		"eventCode": "expert_actual_use", "reportDelay": 0,
		"extVersion": "2.2.8",
		"source":     "mini_program",
		"id":         expertID, "name": expertID, "expertTitle": expertName,
		"type": "send_message", "characterCount": 12, "expertType": expertType,
	}}
}

// SchoolSeasonActivityID 是开学季活动的活动标识（缺它不点亮校园日任务）。
const SchoolSeasonActivityID = "school_open_day_2026"

// ApplyDesktopFingerprint 给事件链注入桌面指纹（导出给 main 包编排使用）。
func ApplyDesktopFingerprint(credential *Credential, events []map[string]any, rt *runtime) []map[string]any {
	return withFingerprint(credential, events, rt)
}

// DesktopExpertSummonSequence 构造专家召唤链（3 事件）。
//
// 关键字段：id 必须是市场真实 ex_ id，type 取专家的首个分类。
func DesktopExpertSummonSequence(expert MarketExpert) []map[string]any {
	category := expert.Category
	if category == "" {
		category = "expert-all"
	}
	version := expert.Version
	if version == "" {
		version = "1.0.0"
	}
	pageURL := "/C:/Program%20Files/WorkBuddy/resources/app.asar/renderer/index.html"
	return []map[string]any{
		{
			"eventCode": "web_element_click", "source": expert.ID, "type": category, "version": version,
			"elementId": "expert_summon_click", "elementName": "立即召唤", "pageURL": pageURL,
		},
		{
			"eventCode": "expert_summon_click", "id": expert.ID, "name": expert.Name,
			"expertTitle": expert.Profession, "type": "expert-all", "position": 0,
			"expertType": expert.Type, "version": version, "mode": "LOCAL",
		},
		{
			"eventCode": "expert_summoned", "id": expert.ID, "name": expert.Name,
			"expertTitle": expert.Profession, "type": "expert-all",
		},
	}
}

// DesktopExpertActualUseEvent 构造专家实际使用事件。
//
// requestId 必须是**真实对话的服务端 id**：自造 UUID 不会被计分。
func DesktopExpertActualUseEvent(expert MarketExpert, conversationID, requestID string) []map[string]any {
	category := expert.Category
	if category == "" {
		category = "expert-all"
	}
	version := expert.Version
	if version == "" {
		version = "1.0.0"
	}
	return []map[string]any{{
		"eventCode": "expert_actual_use", "id": expert.ID, "name": expert.Name,
		"expertTitle": expert.Profession, "type": category, "expertType": expert.Type,
		"source": "builtin", "version": version,
		"cost": 9000, "characterCount": 14,
		"conversationId": conversationID, "requestId": requestID,
		"messageId":      "msg-" + tailOfString(requestID, 8),
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"mode": "craft",
	}}
}

// tailOfString 取字符串末尾 n 个字符。
func tailOfString(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[len(text)-n:]
}

// 本文件生成请求标识与从请求体里提取会话键。

// NewHexID 生成 32 位十六进制随机 ID。
func NewHexID() string {
	return core.RandomHex(16)
}

func newHexID() string {
	return NewHexID()
}

// NewUUIDv7 生成带连字符的标准 UUIDv7（RFC 9562，36 位）。
//
// 官方 5.8.4 客户端的 X-Conversation-ID 要求带连字符的标准 UUID 形态。
func NewUUIDv7() string {
	var buf [16]byte
	if _, errRead := rand.Read(buf[:]); errRead != nil {
		h := core.RandomHex(16)
		return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
	}
	ms := time.Now().UnixMilli()
	buf[0] = byte(ms >> 40)
	buf[1] = byte(ms >> 32)
	buf[2] = byte(ms >> 24)
	buf[3] = byte(ms >> 16)
	buf[4] = byte(ms >> 8)
	buf[5] = byte(ms)
	buf[6] = (buf[6] & 0x0F) | 0x70
	buf[8] = (buf[8] & 0x3F) | 0x80
	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		buf[0], buf[1], buf[2], buf[3],
		buf[4], buf[5],
		buf[6], buf[7],
		buf[8], buf[9],
		buf[10], buf[11], buf[12], buf[13], buf[14], buf[15])
}

// NewUUIDv7Hex 生成去横线的 UUIDv7（32 位十六进制）。
//
// 官方客户端的 conversationRequestId / messageId 都是 UUIDv7 去横线
// （客户端断言正则 ^[0-9a-f]{12}7[0-9a-f]{3}[89ab][0-9a-f]{15}$），
// 纯随机 hex 不是合法 v7 形态。
func NewUUIDv7Hex() string {
	var buf [16]byte
	if _, errRead := rand.Read(buf[:]); errRead != nil {
		// 随机源失败：退回纯随机旧形态，ID 只要求唯一。
		return NewHexID()
	}
	// 48bit 毫秒时间戳 + version 7 + 随机；variant 按 RFC 9562 置 10。
	ms := time.Now().UnixMilli()
	buf[0] = byte(ms >> 40)
	buf[1] = byte(ms >> 32)
	buf[2] = byte(ms >> 24)
	buf[3] = byte(ms >> 16)
	buf[4] = byte(ms >> 8)
	buf[5] = byte(ms)
	buf[6] = (buf[6] & 0x0F) | 0x70
	buf[8] = (buf[8] & 0x3F) | 0x80
	return hex.EncodeToString(buf[:])
}

// ConversationKey 从请求体里提取会话键，用于生成稳定的会话头族。
//
// 识别顺序照抄原实现（粒度由细到粗）：
//  1. metadata.conversation_id / conversationId
//  2. 顶层 conversation_id / conversationId
//  3. prompt_cache_key
//  4. 派生键：system 文本 + 首条 user 消息的哈希
//
// 刻意**不**把 user_id 当会话键：同一用户的所有会话会粘到同一个键上，
// 反而破坏会话隔离。
func ConversationKey(body []byte) string {
	payload := decodeBodyMap(body)
	if payload == nil {
		return ""
	}
	if metadata, okMeta := payload["metadata"].(map[string]any); okMeta {
		for _, key := range []string{"conversation_id", "conversationId"} {
			if value, okValue := metadata[key].(string); okValue && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	for _, key := range []string{"conversation_id", "conversationId"} {
		if value, okValue := payload[key].(string); okValue && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	if value, okValue := payload["prompt_cache_key"].(string); okValue && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return deriveConversationKey(payload)
}

// deriveConversationKey 用 system 与首条 user 消息派生会话键。
//
// 通用 OpenAI 客户端不传任何会话标识，派生键让它们也能享受会话聚合。
// 前缀 "d-" 标明这是派生值（便于与客户端显式传的值区分）。
func deriveConversationKey(payload map[string]any) string {
	messages, okMessages := payload["messages"].([]any)
	if !okMessages || len(messages) == 0 {
		return ""
	}
	systemText := ""
	firstUser := ""
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		role, _ := message["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "system", "developer":
			if systemText == "" {
				systemText = contentSignature(message["content"])
			}
		case "user":
			if firstUser == "" {
				firstUser = contentSignature(message["content"])
			}
		}
	}
	if firstUser == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(systemText + "\x00" + firstUser))
	return "d-" + hex.EncodeToString(sum[:])[:16]
}

// contentSignature 把消息内容压成一个稳定的短签名。
//
// 数组形态（多模态）只取文本部分并保留非文本部分的类型，这样
// 图片消息不会与纯文本消息撞成同一个签名。
func contentSignature(content any) string {
	switch typed := content.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			part, okPart := item.(map[string]any)
			if !okPart {
				continue
			}
			if text, okText := part["text"].(string); okText {
				parts = append(parts, text)
				continue
			}
			partType, _ := part["type"].(string)
			sum := sha256.Sum256([]byte(partType))
			parts = append(parts, "["+partType+":"+hex.EncodeToString(sum[:4])+"]")
		}
		return strings.Join(parts, "\x1f")
	}
	return ""
}

// RealmDisplayName 返回域的展示名。
func RealmDisplayName(region Region) string {
	if region.IsGlobal() {
		return "国际版 (WorkBuddy AI)"
	}
	return "国内版 (CodeBuddy)"
}
