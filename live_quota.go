package codextps

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Codex credit rates, checked 2026-10-15. These are not API USD prices.
// Included-plan capacity is inferred from observed quota movement, not a claim
// that OpenAI denominates a subscription's allowance in purchased credits.
type liveQuotaModelRate struct {
	Model  string  `json:"model"`
	Input  float64 `json:"input"`
	Cached float64 `json:"cached"`
	Output float64 `json:"output"`
}

var liveQuotaModelRates = []liveQuotaModelRate{
	{"gpt-6-astra", 250, 25, 1250},
	{"gpt-6.1-sol", 50, 2.5, 250},
	{"gpt-6-sol", 50, 5, 250},
	{"gpt-6-luna", 2.5, .25, 12.5},
	{"gpt-5.6-sol", 100, 10, 500},
	{"gpt-5.6-terra", 50, 5, 300},
	{"gpt-5.6-luna", 5, .5, 30},
	{"daybreak-blue", 100, 10, 500},
	{"daybreak-red", 312.5, 31.25, 1875},
	{"gpt-rosalind-research", 125, 12.5, 625},
}

type liveQuotaPoint struct {
	At               time.Time `json:"at"`
	UsedPercent      float64   `json:"used_percent"`
	RemainingPercent float64   `json:"remaining_percent"`
	ResetsAt         int64     `json:"resets_at"`
	Segment          int       `json:"segment"`
	WindowMinutes    int64     `json:"window_minutes"`
	Balance          *float64  `json:"credits_balance,omitempty"`
	Expired          bool      `json:"expired"`
	PlanType         string    `json:"plan_type,omitempty"`
	Stale            bool      `json:"stale,omitempty"`
}

type liveQuotaSeries struct {
	ID            string           `json:"id"`
	Bucket        string           `json:"bucket"`
	LimitName     string           `json:"limit_name"`
	Window        string           `json:"window"`
	WindowMinutes int64            `json:"window_minutes"`
	PlanType      string           `json:"plan_type"`
	Points        []liveQuotaPoint `json:"points"`
}

type liveQuotaRules struct {
	AsOf        string               `json:"as_of"`
	Sources     []map[string]string  `json:"sources"`
	Models      []liveQuotaModelRate `json:"models"`
	Basis       string               `json:"basis"`
	Fast        float64              `json:"fast_multiplier"`
	Ultra       float64              `json:"ultra_multiplier"`
	Context     string               `json:"context_policy"`
	ContextNote string               `json:"context_note"`
}

type liveQuotaModelCost struct {
	Model       string  `json:"model"`
	Tier        string  `json:"service_tier"`
	Responses   int     `json:"responses"`
	Input       int64   `json:"input"`
	Cached      int64   `json:"cached"`
	Output      int64   `json:"output"`
	Credits     float64 `json:"credits"`
	LongContext int     `json:"long_context_responses"`
	Unpriced    int     `json:"unpriced"`
}

type liveQuotaTarget struct {
	Model        string       `json:"model"`
	Tier         string       `json:"tier"`
	TokensFull   *float64     `json:"total_tokens_full"`
	TokensRemain *float64     `json:"total_tokens_remaining"`
	MixedRate    *float64     `json:"credits_per_million_mixed_tokens"`
	Mix          liveQuotaMix `json:"mix"`
	LongContext  bool         `json:"long_context"`
}

type liveQuotaMix struct {
	Model          string    `json:"model"`
	Source         string    `json:"source"`
	Responses      int       `json:"responses"`
	MinResponses   int       `json:"min_responses"`
	Sufficient     bool      `json:"sufficient"`
	Fallback       bool      `json:"fallback"`
	From           time.Time `json:"from"`
	To             time.Time `json:"to"`
	NonCached      int64     `json:"non_cached"`
	Cached         int64     `json:"cached"`
	Output         int64     `json:"output"`
	Total          int64     `json:"total"`
	NonCachedRatio float64   `json:"non_cached_ratio"`
	CachedRatio    float64   `json:"cached_ratio"`
	OutputRatio    float64   `json:"output_ratio"`
	LongContext    int       `json:"long_context_responses"`
}

type liveQuotaEstimate struct {
	Status       string               `json:"status"`
	Full         *float64             `json:"full_credits"`
	FullLow      *float64             `json:"full_low"`
	FullHigh     *float64             `json:"full_high"`
	Used         *float64             `json:"used_credits"`
	Remaining    *float64             `json:"remaining_credits"`
	From         time.Time            `json:"from"`
	To           time.Time            `json:"to"`
	DeltaPercent float64              `json:"delta_percent"`
	Credits      float64              `json:"credits"`
	Priced       int                  `json:"priced_responses"`
	Unpriced     int                  `json:"unpriced_responses"`
	UnknownTier  int                  `json:"unknown_tier_responses"`
	LongContext  int                  `json:"long_context_responses"`
	Intervals    int                  `json:"intervals"`
	Notes        []string             `json:"notes"`
	Models       []liveQuotaModelCost `json:"model_breakdown"`
	Target       liveQuotaTarget      `json:"target"`
}

type liveQuotaChange struct {
	At           time.Time `json:"at"`
	FromPercent  float64   `json:"from_percent"`
	ToPercent    float64   `json:"to_percent"`
	DeltaPercent float64   `json:"delta_percent"`
	Credits      float64   `json:"credits"`
	Full         *float64  `json:"full_credits"`
	Priced       int       `json:"priced_responses"`
	Unpriced     int       `json:"unpriced_responses"`
	Segment      int       `json:"segment"`
}

type liveQuotaData struct {
	From         time.Time         `json:"from"`
	To           time.Time         `json:"to"`
	UpdatedAt    time.Time         `json:"updated_at"`
	PendingFiles int               `json:"pending_files"`
	Files        int               `json:"files"`
	HistoryPath  string            `json:"history_path"`
	HistoryError string            `json:"history_error"`
	Series       []liveQuotaSeries `json:"series"`
	Selected     string            `json:"selected_series"`
	Latest       *liveQuotaPoint   `json:"latest"`
	Rules        liveQuotaRules    `json:"rules"`
	Estimate     liveQuotaEstimate `json:"estimate"`
	Changes      []liveQuotaChange `json:"changes"`
}

func quotaRate(model string) (liveQuotaModelRate, bool) {
	for _, rate := range liveQuotaModelRates {
		if model == rate.Model || (strings.HasPrefix(model, rate.Model+"-20") && len(model) == len(rate.Model)+11) {
			return rate, true
		}
	}
	return liveQuotaModelRate{}, false
}

func quotaCost(u liveQuotaUsage, rules liveQuotaRules) (cost float64, priced, long bool) {
	long = u.Tokens.Input > 272000
	rate, ok := quotaRate(u.Model)
	if !ok || u.Tokens.Input < 0 || u.Tokens.Output < 0 || u.Tokens.Cached < 0 || u.Tokens.Cached > u.Tokens.Input {
		return 0, false, long
	}
	multiplier := 1.0
	switch u.ServiceTier {
	case "default":
	case "fast", "priority":
		multiplier = rules.Fast
	case "ultrafast":
		if rate.Model != "gpt-6-astra" && rate.Model != "gpt-6.1-sol" {
			return 0, false, long
		}
		multiplier = rules.Ultra
	default:
		return 0, false, long
	}
	inputMultiplier, outputMultiplier := 1.0, 1.0
	if long && rules.Context == "api_assumption" {
		// This optional scenario is API guidance, not a confirmed Codex credit rule.
		inputMultiplier, outputMultiplier = 2, 1.5
	}
	cost = (float64(u.Tokens.Input-u.Tokens.Cached)*rate.Input*inputMultiplier + float64(u.Tokens.Cached)*rate.Cached*inputMultiplier + float64(u.Tokens.Output)*rate.Output*outputMultiplier) * multiplier / 1e6
	return cost, true, long
}

func quotaRules(r *http.Request) liveQuotaRules {
	rules := liveQuotaRules{AsOf: "2026-10-15", Basis: "included", Fast: 2.5, Ultra: 8, Context: "standard", Models: liveQuotaModelRates,
		ContextNote: "完整上下文输入逐次计入，缓存输入按独立积分价；Codex 未明确公布长上下文额外倍率。当前按标准积分表校准等效容量。",
		Sources: []map[string]string{
			{"title": "Codex 积分表与额度说明", "url": "https://learn.chatgpt.com/docs/pricing"},
			{"title": "Codex Fast / Ultrafast 倍率", "url": "https://learn.chatgpt.com/docs/agent-configuration/speed"},
			{"title": "API 长上下文规则（仅供情景假设）", "url": "https://developers.openai.com/api/docs/pricing"},
		}}
	if r != nil && r.URL.Query().Get("basis") == "credits" {
		rules.Basis, rules.Fast, rules.Ultra = "credits", 2, 6
	}
	if r != nil && r.URL.Query().Get("context") == "api_assumption" {
		rules.Context = "api_assumption"
		rules.ContextNote = "情景假设：单次输入 >272,000 token 时，整次请求输入/缓存 2×、输出 1.5×，再叠加速度倍率。此规则来自 API，未获 Codex 订阅积分口径确认。"
	}
	return rules
}

func newQuotaTarget(r *http.Request) liveQuotaTarget {
	model, tier := r.URL.Query().Get("model"), r.URL.Query().Get("tier")
	if model == "" {
		model = "gpt-6.1-sol"
	}
	if tier == "" {
		tier = "default"
	}
	return liveQuotaTarget{Model: model, Tier: tier, Mix: liveQuotaMix{Source: "insufficient", MinResponses: 10}}
}

func quotaPointer(n float64) *float64 { return &n }

func quotaTargetMix(target liveQuotaTarget, records []liveQuotaRecord, bucket string, rules liveQuotaRules) liveQuotaTarget {
	// Choose the target's observed workload after ten responses. Until then,
	// prefer whichever of Astra and Sol has more actual records in this range.
	// A small sample remains visible as provisional; never invent a fixed mix.
	targetModel := target.Model
	if rate, ok := quotaRate(targetModel); ok {
		targetModel = rate.Model
	}
	byModel := map[string][]liveQuotaRecord{}
	for _, record := range records {
		if record.Bucket != bucket || record.Usage == nil {
			continue
		}
		u := record.Usage
		t := u.Tokens
		if t.Input < 0 || t.Output < 0 || t.Cached < 0 || t.Cached > t.Input || (t.Input == 0 && t.Output == 0) {
			continue
		}
		model := u.Model
		if rate, ok := quotaRate(model); ok {
			model = rate.Model
		}
		if model == targetModel || model == "gpt-6-astra" || model == "gpt-6.1-sol" {
			byModel[model] = append(byModel[model], record)
		}
	}
	model := targetModel
	if len(byModel[model]) < target.Mix.MinResponses {
		reference := "gpt-6-astra"
		if len(byModel["gpt-6.1-sol"]) > len(byModel[reference]) {
			reference = "gpt-6.1-sol"
		}
		if len(byModel[reference]) > 0 {
			model = reference
		}
	}
	samples := byModel[model]
	if len(samples) == 0 {
		return target
	}
	mix := liveQuotaMix{Model: model, Source: "target", Responses: len(samples), MinResponses: target.Mix.MinResponses,
		Sufficient: len(samples) >= target.Mix.MinResponses, Fallback: model != targetModel, From: samples[0].At, To: samples[len(samples)-1].At}
	if mix.Fallback {
		mix.Source = "fallback"
	}
	cost, valid := 0.0, true
	for _, record := range samples {
		t := record.Usage.Tokens
		mix.NonCached += t.Input - t.Cached
		mix.Cached += t.Cached
		mix.Output += t.Output
		// Reprice each real request, preserving its cache split and context
		// length. Applying a long-context rule to an average would be wrong.
		pricedCost, priced, long := quotaCost(liveQuotaUsage{Model: target.Model, ServiceTier: target.Tier, Tokens: t}, rules)
		cost += pricedCost
		valid = valid && priced
		if long {
			mix.LongContext++
		}
	}
	mix.Total = mix.NonCached + mix.Cached + mix.Output
	mix.NonCachedRatio = float64(mix.NonCached) / float64(mix.Total)
	mix.CachedRatio = float64(mix.Cached) / float64(mix.Total)
	mix.OutputRatio = float64(mix.Output) / float64(mix.Total)
	target.Mix, target.LongContext = mix, mix.LongContext > 0
	if valid && cost > 0 {
		target.MixedRate = quotaPointer(cost * 1e6 / float64(mix.Total))
	}
	return target
}

func quotaWindowChanged(a, b liveQuotaPoint) bool {
	return a.WindowMinutes != b.WindowMinutes ||
		(a.PlanType != "" && b.PlanType != "" && a.PlanType != b.PlanType) ||
		(a.ResetsAt > 0) != (b.ResetsAt > 0) ||
		(a.ResetsAt > 0 && b.ResetsAt > 0 && math.Abs(float64(a.ResetsAt-b.ResetsAt)) > 120)
}

func quotaShortRollback(points []liveQuotaPoint, index int, accepted liveQuotaPoint) bool {
	current := points[index]
	if current.UsedPercent >= accepted.UsedPercent || accepted.UsedPercent-current.UsedPercent > 1 || current.At.Sub(accepted.At) > 2*time.Minute || quotaWindowChanged(accepted, current) {
		return false
	}
	// Require observed recovery, rather than assuming every downward move is stale.
	for _, next := range points[index+1:] {
		if next.At.Sub(current.At) > 2*time.Minute || quotaWindowChanged(accepted, next) {
			break
		}
		if next.UsedPercent >= accepted.UsedPercent {
			return true
		}
	}
	return false
}

func (m *liveMonitor) quotaData(r *http.Request, from, to time.Time) liveQuotaData {
	m.mu.Lock()
	defer m.mu.Unlock()
	view := r.URL.Query().Get("view")
	if view == "" {
		view = "default"
	}
	if m.trendViews == nil {
		m.trendViews = map[string]liveTrendRequest{}
	}
	m.trendViews["quota/"+view] = liveTrendRequest{from: from, seen: time.Now()}
	m.updateTrendScope(time.Now())
	data := liveQuotaData{From: from, To: to, UpdatedAt: m.updated, Files: len(m.files), Series: []liveQuotaSeries{}, Rules: quotaRules(r), Changes: []liveQuotaChange{}, Estimate: liveQuotaEstimate{Status: "insufficient", Notes: []string{}, Models: []liveQuotaModelCost{}, Target: newQuotaTarget(r)}}
	if m.discovered.IsZero() {
		data.PendingFiles++
	}
	for _, f := range m.files {
		f.requestSince(m.trendSince)
		if !f.initialized || f.offset < f.size || f.rangeMissing(from) {
			data.PendingFiles++
		}
	}
	if m.quota == nil {
		data.HistoryError = "额度记录尚未初始化"
		return data
	}
	data.HistoryPath, data.HistoryError = m.quota.path, m.quota.errorText
	records := make([]liveQuotaRecord, 0, len(m.quota.records))
	for _, record := range m.quota.records {
		if !record.At.Before(from) && !record.At.After(to) {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].At.Equal(records[j].At) {
			return records[i].ID < records[j].ID
		}
		return records[i].At.Before(records[j].At)
	})
	groups := map[string]*liveQuotaSeries{}
	for _, record := range records {
		for name, window := range map[string]*liveQuotaWindow{"primary": record.Primary, "secondary": record.Secondary} {
			if window == nil {
				continue
			}
			id := record.Bucket + "/" + name
			g := groups[id]
			if g == nil {
				g = &liveQuotaSeries{ID: id, Bucket: record.Bucket, Window: name, LimitName: record.LimitName, PlanType: record.PlanType, Points: []liveQuotaPoint{}}
				groups[id] = g
			}
			g.WindowMinutes, g.PlanType = window.WindowMinutes, record.PlanType
			g.Points = append(g.Points, liveQuotaPoint{At: record.At, UsedPercent: window.UsedPercent, RemainingPercent: max(0, 100-window.UsedPercent), ResetsAt: window.ResetsAt, WindowMinutes: window.WindowMinutes, PlanType: record.PlanType, Balance: record.Balance, Expired: window.ResetsAt > 0 && window.ResetsAt <= time.Now().Unix()})
		}
	}
	for _, g := range groups {
		sort.SliceStable(g.Points, func(i, j int) bool {
			if g.Points[i].At.Equal(g.Points[j].At) {
				return g.Points[i].UsedPercent < g.Points[j].UsedPercent
			}
			return g.Points[i].At.Before(g.Points[j].At)
		})
		segment := 1
		accepted := g.Points[0]
		for i := range g.Points {
			if i > 0 {
				current := g.Points[i]
				if quotaShortRollback(g.Points, i, accepted) {
					g.Points[i].Stale, g.Points[i].Segment = true, segment
					continue
				}
				if current.UsedPercent < accepted.UsedPercent || quotaWindowChanged(accepted, current) || current.At.Sub(accepted.At) > 6*time.Hour {
					segment++
				}
			}
			g.Points[i].Segment = segment
			accepted = g.Points[i]
		}
		data.Series = append(data.Series, *g)
	}
	sort.Slice(data.Series, func(i, j int) bool { return data.Series[i].ID < data.Series[j].ID })
	bucket, window := r.URL.Query().Get("bucket"), r.URL.Query().Get("window")
	if window != "secondary" {
		window = "primary"
	}
	knownBucket := false
	for _, series := range data.Series {
		if series.Bucket == bucket {
			knownBucket = true
		}
	}
	if !knownBucket && len(data.Series) > 0 {
		bucket = data.Series[0].Bucket
	}
	selected := bucket + "/" + window
	data.Selected = selected
	if group := groups[selected]; group != nil && len(group.Points) > 0 {
		last := group.Points[len(group.Points)-1]
		data.Latest = &last
		data.Estimate, data.Changes = estimateQuota(group, records, data.Rules, data.Estimate.Target, data.PendingFiles)
		for i, j := 0, len(data.Changes)-1; i < j; i, j = i+1, j-1 {
			data.Changes[i], data.Changes[j] = data.Changes[j], data.Changes[i]
		}
	}
	for i := range data.Series {
		// Keep both ends of each plateau, without returning thousands of identical points.
		points := data.Series[i].Points
		compact := make([]liveQuotaPoint, 0, len(points))
		for j, point := range points {
			if j > 0 && j+1 < len(points) && !point.Stale && !points[j-1].Stale && !points[j+1].Stale && point.Segment == points[j-1].Segment && point.Segment == points[j+1].Segment && point.UsedPercent == points[j-1].UsedPercent && point.UsedPercent == points[j+1].UsedPercent {
				continue
			}
			compact = append(compact, point)
		}
		data.Series[i].Points = compact
	}
	return data
}

func quotaUsageBetween(records []liveQuotaRecord, bucket string, from, to time.Time, rules liveQuotaRules) (float64, int, int, int, int, []liveQuotaModelCost) {
	var credits float64
	priced, unpriced, unknown, longCount := 0, 0, 0, 0
	groups := map[string]*liveQuotaModelCost{}
	start := sort.Search(len(records), func(i int) bool { return records[i].At.After(from) })
	for _, record := range records[start:] {
		if record.At.After(to) {
			break
		}
		if record.Usage == nil || (record.Bucket != bucket && record.Bucket != "unidentified") {
			continue
		}
		u := *record.Usage
		cost, ok, long := quotaCost(u, rules)
		if record.Bucket != bucket {
			ok = false
		}
		key := u.Model + "/" + u.ServiceTier
		group := groups[key]
		if group == nil {
			group = &liveQuotaModelCost{Model: u.Model, Tier: u.ServiceTier}
			groups[key] = group
		}
		group.Responses++
		group.Input += u.Tokens.Input
		group.Cached += u.Tokens.Cached
		group.Output += u.Tokens.Output
		if long {
			longCount++
			group.LongContext++
		}
		if u.ServiceTier == "" || u.ServiceTier == "unknow" {
			unknown++
		}
		if ok {
			credits += cost
			group.Credits += cost
			priced++
		} else {
			unpriced++
			group.Unpriced++
		}
	}
	models := make([]liveQuotaModelCost, 0, len(groups))
	for _, group := range groups {
		models = append(models, *group)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Credits == models[j].Credits {
			return models[i].Model+models[i].Tier < models[j].Model+models[j].Tier
		}
		return models[i].Credits > models[j].Credits
	})
	return credits, priced, unpriced, unknown, longCount, models
}

func estimateQuota(group *liveQuotaSeries, records []liveQuotaRecord, rules liveQuotaRules, target liveQuotaTarget, pending int) (liveQuotaEstimate, []liveQuotaChange) {
	e := liveQuotaEstimate{Status: "insufficient", Target: quotaTargetMix(target, records, group.Bucket, rules), Notes: []string{}, Models: []liveQuotaModelCost{}}
	changes := []liveQuotaChange{}
	points := group.Points
	anchor := points[0]
	currentStart, currentEnd := points[0], points[0]
	staleCount := 0
	for _, point := range points[1:] {
		if point.Stale {
			staleCount++
			continue
		}
		if point.Segment != anchor.Segment {
			anchor, currentStart, currentEnd = point, point, point
			continue
		}
		if point.UsedPercent <= anchor.UsedPercent {
			continue
		}
		cost, priced, unpriced, _, _, _ := quotaUsageBetween(records, group.Bucket, anchor.At, point.At, rules)
		delta := point.UsedPercent - anchor.UsedPercent
		change := liveQuotaChange{At: point.At, FromPercent: anchor.UsedPercent, ToPercent: point.UsedPercent, DeltaPercent: delta, Credits: cost, Priced: priced, Unpriced: unpriced, Segment: point.Segment}
		if cost > 0 {
			change.Full = quotaPointer(cost * 100 / delta)
		}
		changes = append(changes, change)
		anchor, currentEnd = point, point
	}
	latest := points[len(points)-1]
	e.From, e.To = currentStart.At, currentEnd.At
	e.DeltaPercent = currentEnd.UsedPercent - currentStart.UsedPercent
	if e.DeltaPercent <= 0 {
		e.To = latest.At
	}
	e.Credits, e.Priced, e.Unpriced, e.UnknownTier, e.LongContext, e.Models = quotaUsageBetween(records, group.Bucket, e.From, e.To, rules)
	for _, change := range changes {
		if change.Segment == latest.Segment {
			e.Intervals++
		}
	}
	e.Notes = append(e.Notes, "估算只使用所选范围最新连续额度段；重置、百分比回退、周期变化或超过 6 小时的上报空窗重新建立基线。", "等效积分是模型加权后的本机估计，不是服务端公开的订阅积分余额；假设这些日志属于同一账号，且涵盖同期扣量。其他设备、云任务和共享功能的消耗会使估计偏低。", "百分点上报可能取整并存在延迟；区间仅反映端点合计约 ±1 个百分点的分辨率，不是统计置信区间。")
	if pending > 0 {
		// Backfill is explicit in the UI; do not silently treat missing history as zero.
		e.Notes = append(e.Notes, "历史仍在补读，目前的用量覆盖不完整，估算会随补读变化。")
	}
	if staleCount > 0 {
		e.Notes = append(e.Notes, fmt.Sprintf("所选范围内 %d 条同周期短时回退在 2 分钟内恢复，按迟到快照标记并保留，不参与容量校准；未观察到恢复的回退仍重新分段。", staleCount))
	}
	if latest.Expired {
		e.Notes = append(e.Notes, "最近快照已超过重置时间，剩余量暂不可确认；等待新的额度上报。")
	}
	if latest.ResetsAt <= 0 || latest.WindowMinutes <= 0 {
		e.Notes = append(e.Notes, "当前快照缺少可靠的周期或重置时间，无法确认其窗口边界；请谨慎使用容量估计。")
	}
	if e.Unpriced > 0 {
		e.Notes = append(e.Notes, fmt.Sprintf("区间内 %d 次响应的额度桶、模型或速度档位无法确定，未按零消耗处理；当前点估计只覆盖已知部分，可能明显偏低，上界未知。", e.Unpriced))
	}
	if e.LongContext > 0 && rules.Context == "standard" {
		e.Notes = append(e.Notes, "包含 >272K 上下文请求，已计算其全部输入与缓存；Codex 额外长上下文倍率未确认，可切换 API 情景比较。")
	}
	if e.DeltaPercent > 0 && e.Credits > 0 {
		full := e.Credits * 100 / e.DeltaPercent
		e.Full, e.FullLow = quotaPointer(full), quotaPointer(e.Credits*100/(e.DeltaPercent+1))
		if e.DeltaPercent > 1 && e.Unpriced == 0 {
			e.FullHigh = quotaPointer(e.Credits * 100 / (e.DeltaPercent - 1))
		}
		e.Used = quotaPointer(full * latest.UsedPercent / 100)
		if !latest.Expired {
			e.Remaining = quotaPointer(full * latest.RemainingPercent / 100)
		}
		e.Status = "provisional"
		if e.DeltaPercent >= 3 && e.Unpriced == 0 && pending == 0 && !latest.Expired {
			e.Status = "estimated"
		}
	} else {
		e.Notes = append(e.Notes, "当前连续段还没有可配对的正向百分点变化和可计价响应用量，先记录曲线，等待更多数据。")
	}
	if e.Target.MixedRate != nil {
		costPerToken := *e.Target.MixedRate / 1e6
		if e.Full != nil {
			e.Target.TokensFull = quotaPointer(math.Floor(*e.Full / costPerToken))
		}
		if e.Remaining != nil {
			e.Target.TokensRemain = quotaPointer(math.Floor(*e.Remaining / costPerToken))
		}
	} else if e.Target.Mix.Responses == 0 {
		e.Notes = append(e.Notes, "所选范围内目标模型及 Astra / Sol 都没有有效用量记录，暂不假定缓存、非缓存和输出比例，等待实际样本。")
	} else {
		e.Notes = append(e.Notes, "目标模型或速度组合无法按当前规则计价，指定模型的混合 token 暂不估算。")
	}
	return e, changes
}
