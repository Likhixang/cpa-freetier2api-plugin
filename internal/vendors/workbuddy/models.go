package workbuddy

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/safe"
)

// 本文件实现模型目录：从上游探测模型清单、缓存，并提供窗口与档位的查找链。

// modelCacheEntry 是某个域的模型目录缓存。
type modelCacheEntry struct {
	models    []ModelInfo
	fetchedAt time.Time
	lastFail  time.Time
}

// 缓存窗口。与上游的探测代价匹配：清单变化很慢，但失败后要尽快允许重试。
const (
	modelCacheTTL        = time.Hour
	modelCacheFailWindow = 5 * time.Minute
)

//go:embed assets/catalog_seed.json
var catalogSeedRaw []byte

// ModelInfo 是归一化后的模型元数据。
type ModelInfo struct {
	// ID 是上游模型名（不含插件侧的 realm 前缀）。
	ID string
	// Name 是展示名（上游 display_name，缺失时回落 ID）。
	Name string
	// Description 是模型说明（可能带积分倍率前缀）。
	Description string
	// ContextWindow 是最大输入 token 数。
	ContextWindow int64
	// MaxTokens 是最大输出 token 数（思考与回答共享该预算）。
	MaxTokens int64
	// Efforts 是可选的 reasoning 档位。
	Efforts []string
	// DefaultEffort 是默认档位。
	DefaultEffort string
	// Credits 是积分倍率（形如 "x0.05"）。
	Credits string
	// SupportsImages 表示是否支持图片输入。
	SupportsImages bool
	// SupportsToolCall 表示是否支持工具调用。
	SupportsToolCall bool
	// SupportsReasoning 表示是否支持思维链。
	SupportsReasoning bool
	// CanDisableThinking 表示是否允许关闭思考。
	CanDisableThinking bool
	// OnlyReasoning 表示仅推理（不产出正文）。
	OnlyReasoning bool
	// IsDefault 表示是否为账号默认模型。
	IsDefault bool
}

// catalogSeed 是内嵌的兜底数据。
type catalogSeed struct {
	Context map[string]struct {
		Context   int64 `json:"context"`
		MaxOutput int64 `json:"max_output"`
	} `json:"context"`
	EffortsCN     map[string]effortSeed `json:"efforts_cn"`
	EffortsGlobal map[string]effortSeed `json:"efforts_global"`
}

type effortSeed struct {
	Efforts []string `json:"efforts"`
	Default string   `json:"default"`
}

var (
	seedOnce sync.Once
	seedData catalogSeed
)

func loadSeed() catalogSeed {
	seedOnce.Do(func() {
		if errUnmarshal := json.Unmarshal(catalogSeedRaw, &seedData); errUnmarshal != nil {
			// 内嵌资源损坏是构建期问题，运行期只能降级：没有兜底表仍可工作
			// （窗口与档位字段会缺失，但不影响对话）。
			seedData = catalogSeed{}
		}
	})
	return seedData
}

// dynModelEntry 是上游 /v3/config 与模型目录接口的条目形态。
//
// 字段覆盖 CN 与 global 两种形态：global 会用 modelId/model 作为
// id 的宽松回退键，CN 目录不下发这两个字段。
type dynModelEntry struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	ModelID         string   `json:"modelId"`
	Model           string   `json:"model"`
	Description     string   `json:"descriptionZh"`
	Credits         string   `json:"credits"`
	Tags            []string `json:"tags"`
	Vendor          string   `json:"vendor"`
	IsDefault       bool     `json:"isDefault"`
	MaxInputTokens  int64    `json:"maxInputTokens"`
	MaxOutputTokens int64    `json:"maxOutputTokens"`
	MaxAllowedSize  int64    `json:"maxAllowedSize"`
	Disabled        bool     `json:"disabled"`
	SupportsImages  bool     `json:"supportsImages"`
	SupportsReason  bool     `json:"supportsReasoning"`
	SupportsTool    bool     `json:"supportsToolCall"`
	OnlyReasoning   bool     `json:"onlyReasoning"`
	Reasoning       struct {
		Effort             string   `json:"effort"`
		Summary            string   `json:"summary"`
		DefaultEffort      string   `json:"defaultEffort"`
		CanDisableThinking bool     `json:"canDisableThinking"`
		SupportedEfforts   []string `json:"supportedEfforts"`
	} `json:"reasoning"`
}

// resolvedID 返回条目的模型 ID（带回退链）。
func (e dynModelEntry) resolvedID() string {
	for _, candidate := range []string{e.ID, e.ModelID, e.Model} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return strings.TrimSpace(e.Name)
}

// toModelInfo 把上游条目映射为归一化元数据。
//
// 这是 dynEntry → ModelInfo 的**单一事实来源**：两个域的映射规则必须一致，
// 否则同一模型在 cn 与 global 下会呈现不同的窗口值。
//
// 窗口与档位这里原样透传上游值（可能为 0 / 空），由 applyCatalogFallbacks
// 按 realm 补齐——补齐需要 realm，而映射不需要，两者分开职责更清楚。
func (e dynModelEntry) toModelInfo() ModelInfo {
	id := e.resolvedID()
	name := strings.TrimSpace(e.Name)
	if name == "" {
		name = id
	}
	return ModelInfo{
		ID:                 id,
		Name:               name,
		Description:        strings.TrimSpace(e.Description),
		ContextWindow:      e.MaxInputTokens,
		MaxTokens:          e.MaxOutputTokens,
		Efforts:            normalizeEfforts(e.Reasoning.SupportedEfforts),
		DefaultEffort:      core.FirstNonEmpty(e.Reasoning.DefaultEffort, e.Reasoning.Effort),
		Credits:            strings.TrimSpace(e.Credits),
		SupportsImages:     e.SupportsImages,
		SupportsToolCall:   e.SupportsTool,
		SupportsReasoning:  e.SupportsReason,
		CanDisableThinking: e.Reasoning.CanDisableThinking,
		OnlyReasoning:      e.OnlyReasoning,
		IsDefault:          e.IsDefault,
	}
}

// normalizeEfforts 去重并保持顺序。
func normalizeEfforts(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		effort := strings.ToLower(strings.TrimSpace(item))
		if effort == "" || seen[effort] {
			continue
		}
		seen[effort] = true
		out = append(out, effort)
	}
	return out
}

// nonChatModel 判断条目是否为非对话模型（选中会得到 11102）。
//
// 三类：嵌入/补全/代码专用（id 前缀）、tiny 输出（不是对话模型）、
// 图片生成（tags 标记）。
func nonChatModel(entry dynModelEntry) bool {
	id := strings.ToLower(entry.resolvedID())
	for _, prefix := range []string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	if entry.MaxOutputTokens > 0 && entry.MaxOutputTokens <= 256 {
		return true
	}
	for _, tag := range entry.Tags {
		if strings.EqualFold(strings.TrimSpace(tag), "text-to-image") {
			return true
		}
	}
	return false
}

// FetchModels 返回某个域的模型清单。
//
// 失败时返回缓存（哪怕已过期）而不是错误：上游偶发抖动不该让整个模型列表消失。
// 从未成功过则返回错误，由调用方决定是否暴露给用户。
func (c *Client) FetchModels(region Region) ([]ModelInfo, error) {
	rt := c.rt
	rt.modelMu.Lock()
	entry, hasCache := rt.modelCache[region]
	rt.modelMu.Unlock()

	if hasCache && len(entry.models) > 0 {
		if time.Since(entry.fetchedAt) < modelCacheTTL {
			return entry.models, nil
		}
		// 失败冷却期内不重复打上游。
		if time.Since(entry.lastFail) < modelCacheFailWindow {
			return entry.models, nil
		}
	}

	models, errFetch := c.fetchModelsFromUpstream(region)
	if errFetch != nil {
		rt.modelMu.Lock()
		cached := rt.modelCache[region]
		cached.lastFail = time.Now()
		if len(cached.models) == 0 {
			cached.models = entry.models
		}
		rt.modelCache[region] = cached
		rt.modelMu.Unlock()
		if len(cached.models) > 0 {
			return cached.models, nil
		}
		return nil, errFetch
	}

	rt.modelMu.Lock()
	rt.modelCache[region] = modelCacheEntry{models: models, fetchedAt: time.Now()}
	rt.modelMu.Unlock()
	return models, nil
}

// 模型目录缓存挂在 adapter 的 runtime 上，按 App 实例隔离。
//
// Client 是一次操作用完即弃的，缓存不能挂在 Client 上；但也不能放包级，
// 否则两个 App 实例会共享同一份目录。runtime 的生命周期与 adapter 一致，
// 既避免重复探测，又保证实例隔离。

// cachedModels 只返回已缓存的清单，不触发上游请求（供管理页展示）。
func (r *runtime) cachedModels() map[Region][]ModelInfo {
	r.modelMu.Lock()
	defer r.modelMu.Unlock()
	out := make(map[Region][]ModelInfo, len(r.modelCache))
	for region, entry := range r.modelCache {
		if len(entry.models) > 0 {
			out[region] = entry.models
		}
	}
	return out
}

// loadCachedModels 从状态文件加载上次缓存的模型清单。
//
// 启动时先灌缓存，可以让 /v1/models 在首次上游探测完成前就有内容，
// 避免客户端启动瞬间看到空列表。
func (r *runtime) loadCachedModels(cached []ModelInfo, realm Region) {
	if len(cached) == 0 {
		return
	}
	r.modelMu.Lock()
	defer r.modelMu.Unlock()
	if len(r.modelCache[realm].models) > 0 {
		return
	}
	r.modelCache[realm] = modelCacheEntry{models: cached}
}

// applyCatalogFallbacks 用查找链补齐窗口与档位。
//
// 四级查找链（窗口）：上游动态值 → 静态兜底表 (catalog_seed) → 默认值。
// 档位是三级链：上游动态值 → realm 分表静态兜底 → 省略字段。
//
// 两套链刻意不同：窗口有「宁可高估」的安全侧（低估会让客户端过早截断上下文），
// 而档位没有安全侧（发错档位会被上游 400 拒绝），所以档位查不到就省略。
func applyCatalogFallbacks(models []ModelInfo, region Region) []ModelInfo {
	seed := loadSeed()
	out := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		if model.ContextWindow <= 0 {
			if cap, okCap := seed.Context[model.ID]; okCap {
				model.ContextWindow = cap.Context
				if model.MaxTokens <= 0 {
					model.MaxTokens = cap.MaxOutput
				}
			}
		}
		if model.ContextWindow <= 0 {
			model.ContextWindow = defaultContextWindow
		}
		// 档位：上游没给才回落静态分表。上游给了就以它为准，绝不叠加。
		if len(model.Efforts) == 0 {
			if cap, okCap := staticEffortCap(region, model.ID); okCap {
				model.Efforts = cap.Efforts
				if model.DefaultEffort == "" {
					model.DefaultEffort = cap.DefaultEffort
				}
			}
		}
		// defaultEffort 必须属于 efforts，否则客户端会发出非法档位。
		if model.DefaultEffort != "" && !containsString(model.Efforts, model.DefaultEffort) {
			model.DefaultEffort = ""
		}
		out = append(out, model)
	}
	return out
}

// defaultContextWindow 是窗口查找链的最终兜底。
//
// 上游模型普遍是 1M 上下文，低估会让客户端过早截断对话。
const defaultContextWindow int64 = 1000000

// LoadCachedModelsFromState 用 state.json 里的缓存预热模型目录。
//
// 缓存结构由 core 定义（state 只是它的落盘容器），这里只负责按区域预热，
// 不再在这里重复一遍 WorkBuddy 的字段映射。
func (r *runtime) loadCachedModelsFromState(cached []core.CachedModel) {
	byRegion := map[Region][]ModelInfo{}
	for _, item := range cached {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		region := NormalizeRegion(item.Realm)
		byRegion[region] = append(byRegion[region], ModelInfo{
			ID:                 item.ID,
			Name:               item.Name,
			ContextWindow:      item.ContextLength,
			MaxTokens:          item.MaxOutputTokens,
			Efforts:            item.Efforts,
			DefaultEffort:      item.DefaultEffort,
			SupportsImages:     item.SupportsImages,
			SupportsToolCall:   item.SupportsTool,
			CanDisableThinking: item.CanDisableThink,
			OnlyReasoning:      item.OnlyReasoning,
			Description:        item.Description,
			Credits:            item.Credits,
		})
	}
	for region, models := range byRegion {
		r.loadCachedModels(models, region)
	}
}

// staticEffortCap 按 realm 查静态档位兜底表。
//
// **必须按 realm 分表**：同一个模型名在两侧的合法档位不同
// （deepseek-v4.1-flash 在 CN 是 low/high/max，在 global 只有 high），
// 混用会让 global 请求带上 low/max 而被上游 400 拒绝。
func staticEffortCap(region Region, modelID string) (ModelInfo, bool) {
	seed := loadSeed()
	table := seed.EffortsCN
	if region.IsGlobal() {
		table = seed.EffortsGlobal
	}
	entry, okEntry := table[strings.ToLower(strings.TrimSpace(modelID))]
	if !okEntry {
		return ModelInfo{}, false
	}
	return ModelInfo{Efforts: normalizeEfforts(entry.Efforts), DefaultEffort: entry.Default}, true
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// sortModels 按 ID 排序，保证 /v1/models 输出稳定。
func sortModels(models []ModelInfo) {
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
}

// 本文件实现模型清单的上游探测。
//
// CN 与 global 的探测路径不同，且**都是并发多路 + 并集合并**：
//   - CN：/v3/config（IDE UA）+ 企业模型目录；
//   - global：/v3/config（IDE UA）+ /v3/config（CLI UA）+ 企业模型目录家族。
//
// 两路/三路缺一不可：实测 IDE UA 与 CLI UA 返回的清单**各有独有模型**
// （IDE 有 o4-mini/enhance-1.0，CLI 有 deepseek 系），任何单路都会漏模型。
//
// 合并策略：v3 条目优先（字段更全），企业端点只补 v3 缺失的 id。
// 单路失败降级为另一路；全部失败才返回错误。

// v3ConfigResponse 是 /v3/config 的响应体。
//
// 注意：doJSON 已经解开了一层信封（code / msg / data），
// 因此这里对应的是 data 的内容，不带 code 字段。
type v3ConfigResponse struct {
	Models []dynModelEntry `json:"models"`
	// ProductFeaturesConfig 里放「试用模型横幅」：上游把限时试用的模型
	// 放在这里而**不在 models 里**，但那些模型实际可调用。
	ProductFeaturesConfig struct {
		ModelTrialBanner struct {
			Banners []struct {
				ModelID       string `json:"modelId"`
				TargetModelID string `json:"targetModelId"`
			} `json:"banners"`
		} `json:"ModelTrialBanner"`
	} `json:"productFeaturesConfig"`
}

// enterpriseModelsResponse 是企业模型目录的响应体（同样已被解开一层信封）。
type enterpriseModelsResponse struct {
	// Agents 给出模型的白名单与顺序（cli agent 的 models 列表）。
	Agents []struct {
		Name   string   `json:"name"`
		Models []string `json:"models"`
	} `json:"agents"`
	// Models 是字段来源，按 id 取。
	Models []dynModelEntry `json:"models"`
}

// fetchModelsFromUpstream 并发探测某个域的模型清单。
func (c *Client) fetchModelsFromUpstream(region Region) ([]ModelInfo, error) {
	if !c.enabledRealm(region) {
		return nil, fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}

	type probeResult struct {
		// primary 为 true 表示该路是 v3 目录（字段更全，合并时优先）。
		primary bool
		models  []ModelInfo
		err     error
	}
	var (
		waitGroup sync.WaitGroup
		mu        sync.Mutex
		results   []probeResult
	)

	record := func(primary bool, models []ModelInfo, err error) {
		mu.Lock()
		results = append(results, probeResult{primary: primary, models: models, err: err})
		mu.Unlock()
	}

	// probe 并发执行一路探测；其中的 panic 记为这一路的错误，其余路照常合并。
	probe := func(primary bool, scope string, fetch func() ([]ModelInfo, error)) {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			var models []ModelInfo
			errProbe := safe.Call("workbuddy model probe "+scope, func() error {
				var errFetch error
				models, errFetch = fetch()
				return errFetch
			})
			record(primary, models, errProbe)
		}()
	}

	// /v3/config（IDE UA）：两域共用，字段最全。
	probe(true, "v3 ide", func() ([]ModelInfo, error) { return c.probeV3Config(region, codeBuddyIDEUA) })

	if region.IsGlobal() {
		// /v3/config（CLI UA）：仅 global。独有的 deepseek 系模型只在这条路上。
		probe(true, "v3 cli", func() ([]ModelInfo, error) { return c.probeV3Config(region, codeBuddyCLIUA) })
	}

	// 企业模型目录：v3 缺失的 id 由它补齐。
	probe(false, "enterprise", func() ([]ModelInfo, error) { return c.probeEnterpriseModels(region) })

	waitGroup.Wait()

	// 先合并两路 v3（IDE 结果在前，字段权威），再用企业端点补缺失。
	var primaryModels, fallbackModels []ModelInfo
	var firstErr error
	for _, result := range results {
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		if result.primary {
			primaryModels = append(primaryModels, result.models...)
		} else {
			fallbackModels = append(fallbackModels, result.models...)
		}
	}

	merged := dedupeModels(primaryModels)
	// 企业端点只补 v3 没有的 id（不覆盖已有字段）。
	for _, model := range fallbackModels {
		if !containsModelID(merged, model.ID) {
			merged = append(merged, model)
		}
	}
	if len(merged) == 0 {
		if firstErr != nil {
			return nil, fmt.Errorf("probe models for %s: %w", region, firstErr)
		}
		return nil, fmt.Errorf("probe models for %s: no models returned", region)
	}

	merged = filterChatModels(merged)
	if len(merged) == 0 {
		return nil, fmt.Errorf("probe models for %s: all entries filtered as non-chat", region)
	}
	merged = applyCatalogFallbacks(merged, region)
	sortModels(merged)
	return merged, nil
}

// probeCredential 取一个用于探测的凭证。
func (c *Client) probeCredential(region Region) *Credential {
	if cred := c.rt.hostProbeCredential(region); cred != nil {
		return cred
	}
	return &Credential{}
}

// probeV3Config 拉取 /v3/config 并解析模型清单。
func (c *Client) probeV3Config(region Region, userAgent string) ([]ModelInfo, error) {
	// 探测需要一个凭证：没有凭证就无法请求上游。
	cred := c.probeCredential(region)
	if cred == nil {
		return nil, fmt.Errorf("no credential available for model probe")
	}
	endpoints := GetEndpoints(region)
	req, errReq := http.NewRequestWithContext(c.opts.Context, http.MethodGet, endpoints.ChatBase+v3ConfigPath, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build v3 config request: %w", errReq)
	}
	c.applyV3ConfigHeaders(req, cred, userAgent)

	data, errDo := c.doJSONWithLimit(req, configBodyLimit)
	if errDo != nil {
		return nil, errDo
	}

	var payload v3ConfigResponse
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode v3 config: %w", errUnmarshal)
	}
	models := make([]ModelInfo, 0, len(payload.Models))
	for _, entry := range payload.Models {
		if entry.Disabled || nonChatModel(entry) {
			continue
		}
		models = append(models, entry.toModelInfo())
	}
	// 试用模型横幅：实际可调用但不在 data.models 里，元数据从 target 继承，
	// 但计费与营销字段必须清空（它们描述的是「转正后」的状态）。
	for _, banner := range payload.ProductFeaturesConfig.ModelTrialBanner.Banners {
		trialID := strings.TrimSpace(banner.ModelID)
		if trialID == "" || containsModelID(models, trialID) {
			continue
		}
		targetID := strings.TrimSpace(banner.TargetModelID)
		trial := ModelInfo{ID: trialID, Name: trialID}
		if base, okBase := findModel(models, targetID); okBase {
			trial = base
			trial.ID = trialID
			trial.Name = trialID
			trial.Credits = ""
		}
		models = append(models, trial)
	}
	return models, nil
}

// probeEnterpriseModels 拉取企业模型目录。
func (c *Client) probeEnterpriseModels(region Region) ([]ModelInfo, error) {
	cred := c.probeCredential(region)
	if cred == nil {
		return nil, fmt.Errorf("no credential available for model probe")
	}
	endpoints := GetEndpoints(region)
	paths := []string{enterpriseModelsPathCN}
	if region.IsGlobal() {
		paths = []string{enterpriseModelsPathGlobal, enterpriseModelsPathCN}
	}

	var lastErr error
	for _, path := range paths {
		req, errReq := http.NewRequestWithContext(c.opts.Context, http.MethodGet, endpoints.ChatBase+path, nil)
		if errReq != nil {
			lastErr = errReq
			continue
		}
		c.applyV3ConfigHeaders(req, cred, codeBuddyIDEUA)

		data, errDo := c.doJSONWithLimit(req, configBodyLimit)
		if errDo != nil {
			lastErr = errDo
			// 仅在 404 时换下一条路径（其他错误换路径也救不回来）。
			if upstreamErr, okErr := errDo.(*core.Failure); okErr && upstreamErr.Kind == core.FailureKindNotFound {
				continue
			}
			return nil, errDo
		}

		var payload enterpriseModelsResponse
		if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
			lastErr = fmt.Errorf("decode enterprise models: %w", errUnmarshal)
			continue
		}
		return parseEnterpriseModels(payload), nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("enterprise models endpoint is unavailable")
}

// parseEnterpriseModels 按「agents 白名单顺序 + models 字段来源」解析企业目录。
//
// 白名单顺序是刻意的：上游用它表达模型的推荐排序，丢了顺序等于丢了产品意图。
func parseEnterpriseModels(payload enterpriseModelsResponse) []ModelInfo {
	byID := make(map[string]dynModelEntry, len(payload.Models))
	for _, entry := range payload.Models {
		if id := entry.resolvedID(); id != "" {
			byID[id] = entry
		}
	}

	order := make([]string, 0, len(byID))
	seen := make(map[string]bool, len(byID))
	for _, agent := range payload.Agents {
		if !strings.EqualFold(strings.TrimSpace(agent.Name), "cli") {
			continue
		}
		for _, id := range agent.Models {
			trimmed := strings.TrimSpace(id)
			if trimmed == "" || seen[trimmed] {
				continue
			}
			seen[trimmed] = true
			order = append(order, trimmed)
		}
	}
	// 没有 agent 白名单时退回 models 的原序。
	if len(order) == 0 {
		for _, entry := range payload.Models {
			if id := entry.resolvedID(); id != "" && !seen[id] {
				seen[id] = true
				order = append(order, id)
			}
		}
	}

	out := make([]ModelInfo, 0, len(order))
	for _, id := range order {
		entry, okEntry := byID[id]
		if !okEntry {
			// 白名单里有、字段表里没有：只有 id 可用，窗口交给查找链兜底。
			out = append(out, ModelInfo{ID: id, Name: id})
			continue
		}
		if entry.Disabled || nonChatModel(entry) {
			continue
		}
		out = append(out, entry.toModelInfo())
	}
	return out
}

// dedupeModels 按 ID 去重，保留首次出现的条目。
func dedupeModels(models []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, model)
	}
	return out
}

// filterChatModels 剔除对话不可用的模型。
func filterChatModels(models []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		// tiny 输出的条目在上游条目层已被过滤，这里再兜一层：
		// 动态兜底解析出的条目没有原始 tags，只能靠输出上限判断。
		if model.MaxTokens > 0 && model.MaxTokens <= 256 {
			continue
		}
		out = append(out, model)
	}
	return out
}

func containsModelID(models []ModelInfo, id string) bool {
	_, okFound := findModel(models, id)
	return okFound
}

func findModel(models []ModelInfo, id string) (ModelInfo, bool) {
	for _, model := range models {
		if model.ID == id {
			return model, true
		}
	}
	return ModelInfo{}, false
}
