package codextps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The live reader reuses codex-tps' response classification and ResponseSample
// timing model, but keeps untimed usage as well. Missing timing is never zero TPS.
type liveTokens struct {
	Input      int64 `json:"input"`
	Cached     int64 `json:"cached"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
	Visible    int64 `json:"visible"`
}

func tokensFrom(m map[string]any) liveTokens {
	x := liveTokens{Input: integer(m, "input_tokens"), Cached: integer(m, "cached_input_tokens"), CacheWrite: integer(m, "cache_write_input_tokens"), Output: integer(m, "output_tokens"), Reasoning: integer(m, "reasoning_output_tokens")}
	x.Visible = max(0, x.Output-x.Reasoning)
	return x
}
func (x *liveTokens) add(y liveTokens) {
	x.Input += y.Input
	x.Cached += y.Cached
	x.CacheWrite += y.CacheWrite
	x.Output += y.Output
	x.Reasoning += y.Reasoning
	x.Visible += y.Visible
}
func (x liveTokens) delta(y liveTokens) (liveTokens, bool) {
	z := liveTokens{Input: x.Input - y.Input, Cached: x.Cached - y.Cached, CacheWrite: x.CacheWrite - y.CacheWrite, Output: x.Output - y.Output, Reasoning: x.Reasoning - y.Reasoning}
	ok := z.Input >= 0 && z.Cached >= 0 && z.CacheWrite >= 0 && z.Output >= 0 && z.Reasoning >= 0
	z.Visible = max(0, z.Output-z.Reasoning)
	return z, ok
}
func (x liveTokens) empty() bool {
	return x.Input == 0 && x.Cached == 0 && x.CacheWrite == 0 && x.Output == 0 && x.Reasoning == 0
}

type liveMetrics struct {
	Tokens             liveTokens `json:"tokens"`
	TimedTokens        liveTokens `json:"timed_tokens"`
	Seconds            float64    `json:"response_seconds"`
	Responses          int        `json:"responses"`
	TimedResponses     int        `json:"timed_responses"`
	ActualTokens       int64      `json:"actual_tokens"`
	ActualSeconds      float64    `json:"actual_seconds"`
	ActualResponses    int        `json:"actual_responses"`
	EstimatedResponses int        `json:"estimated_responses"`
}

func (m *liveMetrics) add(r liveResponse) {
	m.Tokens.add(r.Tokens)
	m.Responses++
	if r.Seconds > 0 {
		m.TimedTokens.add(r.Tokens)
		m.Seconds += r.Seconds
		m.TimedResponses++
	}
	if r.GenerationSeconds > 0 && r.TPS.Actual != nil {
		m.ActualTokens += r.Tokens.Output
		m.ActualSeconds += r.GenerationSeconds
		m.ActualResponses++
		if r.TTFT != nil && r.TTFT.Estimated {
			m.EstimatedResponses++
		}
	}
}

func (m *liveMetrics) merge(other liveMetrics) {
	m.Tokens.add(other.Tokens)
	m.TimedTokens.add(other.TimedTokens)
	m.Seconds += other.Seconds
	m.Responses += other.Responses
	m.TimedResponses += other.TimedResponses
	m.ActualTokens += other.ActualTokens
	m.ActualSeconds += other.ActualSeconds
	m.ActualResponses += other.ActualResponses
	m.EstimatedResponses += other.EstimatedResponses
}
func ratio(n float64, d float64) *float64 {
	if d <= 0 {
		return nil
	}
	v := n / d
	return &v
}

type liveRates struct {
	Output          *float64 `json:"output"`
	Visible         *float64 `json:"visible"`
	Reasoning       *float64 `json:"reasoning"`
	Perceived       *float64 `json:"perceived"`
	Actual          *float64 `json:"actual"`
	ActualEstimated bool     `json:"actual_estimated"`
}

func (m liveMetrics) rates() liveRates {
	return liveRates{Output: ratio(float64(m.TimedTokens.Output), m.Seconds), Visible: ratio(float64(m.TimedTokens.Visible), m.Seconds), Reasoning: ratio(float64(m.TimedTokens.Reasoning), m.Seconds), Perceived: ratio(float64(m.TimedTokens.Output), m.Seconds), Actual: ratio(float64(m.ActualTokens), m.ActualSeconds), ActualEstimated: m.EstimatedResponses > 0}
}

type liveResponse struct {
	Index             int              `json:"index"`
	TurnIndex         int              `json:"turn_index"`
	TurnID            string           `json:"turn_id"`
	Model             string           `json:"model"`
	Effort            string           `json:"effort"`
	ServiceTier       string           `json:"service_tier"`
	ServiceTierSource string           `json:"service_tier_source,omitempty"`
	StartedAt         time.Time        `json:"started_at"`
	CompletedAt       time.Time        `json:"completed_at"`
	FirstOutputAt     time.Time        `json:"first_output_at"`
	RecordedAt        time.Time        `json:"recorded_at"`
	Seconds           float64          `json:"seconds"`
	Tokens            liveTokens       `json:"tokens"`
	TPS               liveRates        `json:"tps"`
	TimingNote        string           `json:"timing_note,omitempty"`
	Credits           *liveCreditUsage `json:"credits,omitempty"`
	TTFT              *liveFirstToken  `json:"ttft,omitempty"`
	GenerationSeconds float64          `json:"generation_seconds"`
	Transport         string           `json:"transport"`
	TransportFallback bool             `json:"transport_fallback"`
	TransportInferred bool             `json:"transport_inferred"`
	TransportNote     string           `json:"transport_note"`
}
type liveTurn struct {
	ID                 string         `json:"id"`
	Number             int            `json:"number"`
	Title              string         `json:"title"`
	Model              string         `json:"model"`
	Effort             string         `json:"effort"`
	ServiceTier        string         `json:"service_tier"`
	ServiceTierSource  string         `json:"service_tier_source,omitempty"`
	Status             string         `json:"status"`
	StartedAt          time.Time      `json:"started_at"`
	EndedAt            time.Time      `json:"ended_at"`
	TimeToFirstTokenMS *int64         `json:"time_to_first_token_ms"`
	Metrics            liveMetrics    `json:"metrics"`
	Responses          []liveResponse `json:"-"`
}
type liveSession struct {
	ID                  string
	metadataSet         bool
	legacyID            string
	hasInheritedMeta    bool
	inheritedContext    bool
	Title               string
	CWD                 string
	ParentID            string
	AgentNickname       string
	AgentRole           string
	AgentPath           string
	CreatedAt           time.Time
	UpdatedAt           time.Time
	LastResponseAt      time.Time
	Turns               []*liveTurn
	Metrics             liveMetrics
	Warnings            []string
	turnsByID           map[string]*liveTurn
	current             *liveTurn
	model, effort       string
	serviceTier         string
	serviceTiers        []liveTierSetting
	start, generatedEnd *time.Time
	firstGenerated      *time.Time
	sawStart            bool
	lastTotal           *liveTokens
	lastUsageKey        string
	quotaPending        []liveQuotaRecord
}

func newLiveSession(id string) *liveSession {
	return &liveSession{ID: id, turnsByID: map[string]*liveTurn{}, Turns: []*liveTurn{}, Warnings: []string{}}
}
func (s *liveSession) agentName() string {
	name := strings.TrimRight(s.AgentPath, "/")
	if name != "" {
		return name[strings.LastIndex(name, "/")+1:]
	}
	return s.AgentNickname
}
func (s *liveSession) warn(message string) {
	for _, old := range s.Warnings {
		if old == message {
			return
		}
	}
	if len(s.Warnings) < 12 {
		s.Warnings = append(s.Warnings, message)
	}
}
func shortText(text string, length int) string {
	r := []rune(strings.Join(strings.Fields(text), " "))
	if len(r) > length {
		return string(r[:length]) + "…"
	}
	return string(r)
}
func messageText(p map[string]any) string {
	if v, ok := p["message"].(string); ok {
		return v
	}
	if v, ok := p["content"].(string); ok {
		return v
	}
	parts, _ := p["content"].([]any)
	for _, part := range parts {
		if m, ok := part.(map[string]any); ok {
			if text, ok := m["text"].(string); ok && text != "" {
				return text
			}
		}
	}
	return ""
}
func (s *liveSession) turn(id string, at time.Time) *liveTurn {
	if id == "" {
		id = "unattributed"
	}
	if t := s.turnsByID[id]; t != nil {
		s.current = t
		return t
	}
	t := &liveTurn{ID: id, Number: len(s.Turns) + 1, Title: "等待用户轮次信息", StartedAt: at, Status: "active", Responses: []liveResponse{}}
	if id == "unattributed" {
		t.Title = "缺少轮次标识的用量"
		t.Status = "unknown"
	}
	s.Turns = append(s.Turns, t)
	s.turnsByID[id] = t
	s.current = t
	return t
}
func (s *liveSession) setTitle(p map[string]any) {
	raw := strings.TrimSpace(messageText(p))
	// Codex persists bootstrap/context envelopes as user-role messages too.
	// Use actual request text for navigation, rather than injected workspace metadata.
	for _, prefix := range []string{"# AGENTS.md instructions", "## Referenced chats with Codex:", "<environment_context>", "<external_codex_apps_open_page>", "<send_user_message_question_reply>", "<recommended_plugins>", "<permissions instructions>", "<skills_instructions>", "<codex_apps_"} {
		if strings.HasPrefix(raw, prefix) {
			return
		}
	}
	text := shortText(raw, 100)
	if text == "" {
		return
	}
	if s.current != nil && s.current.Title == "等待用户轮次信息" {
		s.current.Title = text
	}
	if s.Title == "" {
		s.Title = text
	}
}
func (s *liveSession) consume(line []byte, lineNumber int) {
	var row map[string]any
	d := json.NewDecoder(bytes.NewReader(line))
	d.UseNumber()
	if err := d.Decode(&row); err != nil {
		s.warn(fmt.Sprintf("当前加载段第 %d 行 JSON 无法解析，已跳过；其余日志继续采集", lineNumber))
		return
	}
	at, err := time.Parse(time.RFC3339Nano, textField(row, "timestamp"))
	if err != nil {
		s.warn("部分日志缺少有效时间戳，无法纳入统计")
		return
	}
	if at.After(s.UpdatedAt) {
		s.UpdatedAt = at
	}
	p, _ := row["payload"].(map[string]any)
	kind := textField(row, "type")
	event := textField(p, "type")
	// Imported histories contain their own task/turn events. Only a setting
	// explicitly addressed to this thread ends that inherited context.
	if s.inheritedContext && kind != "session_meta" {
		if kind != "event_msg" || event != "thread_settings_applied" || textField(p, "thread_id") != s.ID {
			return
		}
		s.inheritedContext = false
	}
	switch {
	case kind == "session_meta":
		if id := textField(p, "id"); id != "" {
			// Keep the old parser's checkpoint identity solely for history lookup.
			s.legacyID = id
			if s.metadataSet && id != s.ID {
				s.hasInheritedMeta, s.inheritedContext = true, true
				return
			}
			s.ID = id
			s.metadataSet = true
			s.inheritedContext = false
		}
		s.CWD = textField(p, "cwd")
		s.CreatedAt = at
		s.ParentID = textField(p, "parent_thread_id")
		s.AgentNickname = textField(p, "agent_nickname")
		s.AgentRole = textField(p, "agent_role")
		s.AgentPath = textField(p, "agent_path")
		if source, ok := p["source"].(map[string]any); ok {
			if sub, ok := source["subagent"].(map[string]any); ok {
				if spawn, ok := sub["thread_spawn"].(map[string]any); ok {
					if s.ParentID == "" {
						s.ParentID = textField(spawn, "parent_thread_id")
					}
					if s.AgentNickname == "" {
						s.AgentNickname = textField(spawn, "agent_nickname")
					}
					if s.AgentRole == "" {
						s.AgentRole = textField(spawn, "agent_role")
					}
					if s.AgentPath == "" {
						s.AgentPath = textField(spawn, "agent_path")
					}
				}
			}
		}
	case kind == "event_msg" && event == "thread_settings_applied":
		if id := textField(p, "thread_id"); id != "" && id != s.ID {
			return
		}
		settings, _ := p["thread_settings"].(map[string]any)
		s.applyServiceTier(textField(settings, "service_tier"), at)
	case kind == "event_msg" && event == "task_started":
		id := textField(p, "turn_id")
		if id == "" {
			id = fmt.Sprintf("local-%d", lineNumber)
		}
		if s.current != nil && s.current.ID != id && s.current.Status == "active" {
			s.current.Status = "interrupted"
			s.current.EndedAt = at
		}
		t := s.turn(id, at)
		t.Status = "active"
		t.TimeToFirstTokenMS = nil
		t.ServiceTier = s.serviceTier
		if len(s.serviceTiers) > 0 {
			t.ServiceTierSource = "logged"
		}
		s.start, s.generatedEnd, s.firstGenerated = timePointer(at), nil, nil
		s.sawStart = true
	case kind == "turn_context":
		if id := textField(p, "turn_id"); id != "" {
			s.turn(id, at)
		}
		if model := textField(p, "model"); model != "" {
			s.model = model
		}
		if effort := textField(p, "effort"); effort != "" {
			s.effort = effort
		}
		if s.current != nil {
			s.current.Model = s.model
			s.current.Effort = s.effort
			s.current.ServiceTier = s.serviceTier
			if len(s.serviceTiers) > 0 {
				s.current.ServiceTierSource = "logged"
			}
		}
		if s.generatedEnd == nil {
			s.start = timePointer(at)
		}
	case kind == "event_msg" && (event == "task_complete" || event == "turn_aborted"):
		t := s.current
		if id := textField(p, "turn_id"); id != "" {
			t = s.turnsByID[id]
		}
		if t != nil {
			t.Status = "completed"
			if event == "turn_aborted" {
				t.Status = "interrupted"
			}
			t.EndedAt = at
			if value, ok := p["time_to_first_token_ms"].(json.Number); ok {
				if milliseconds, err := value.Int64(); err == nil && milliseconds >= 0 {
					t.TimeToFirstTokenMS = &milliseconds
				}
			}
		}
	case kind == "event_msg" && event == "user_message":
		if s.current == nil {
			s.turn("", at)
		}
		s.setTitle(p)
		if s.generatedEnd == nil {
			s.start = timePointer(at)
		}
	case kind == "response_item":
		if textField(p, "role") == "user" {
			if s.current == nil {
				s.turn("", at)
			}
			s.setTitle(p)
		}
		if isGeneratedItem(p) {
			if s.firstGenerated == nil {
				s.firstGenerated = timePointer(at)
			}
			s.generatedEnd = timePointer(at)
		} else if s.generatedEnd == nil && isInputItem(p) {
			s.start = timePointer(at)
		}
	case kind == "event_msg" && event == "token_count":
		previous := s.Metrics.Responses
		s.consumeUsage(p, at)
		s.captureQuota(p, at, previous)
	}
}
func textField(m map[string]any, key string) string { v, _ := m[key].(string); return v }
func (s *liveSession) consumeUsage(p map[string]any, at time.Time) {
	info, _ := p["info"].(map[string]any)
	if info == nil {
		return
	}
	last, _ := info["last_token_usage"].(map[string]any)
	total, _ := info["total_token_usage"].(map[string]any)
	if last == nil && total == nil {
		return
	}
	usage := tokensFrom(last)
	if total != nil {
		current := tokensFrom(total)
		if s.lastTotal != nil {
			delta, monotonic := current.delta(*s.lastTotal)
			if monotonic && delta.empty() {
				return
			} // quota-only/repeated checkpoints
			if monotonic {
				usage = delta
			} else {
				s.warn("累计计数发生重置，重置边界按最近响应用量计入")
			}
		} else if last == nil {
			s.warn("首个累计检查点缺少最近响应用量，已作为基线而不归入当前轮次")
			s.lastTotal = &current
			s.start, s.generatedEnd, s.firstGenerated = timePointer(at), nil, nil
			return
		}
		s.lastTotal = &current
	} else {
		key := fmt.Sprintf("%s/%v", at.Format(time.RFC3339Nano), usage)
		if key == s.lastUsageKey {
			return
		}
		s.lastUsageKey = key
		if s.generatedEnd == nil && usage == tokensFrom(last) {
			s.warn("部分用量仅有最近响应字段，重复记录的识别精度受日志限制")
		}
	}
	if usage.empty() {
		return
	}
	if s.current == nil {
		s.turn("", at)
	}
	t := s.current
	r := liveResponse{Index: s.Metrics.Responses + 1, TurnIndex: len(t.Responses) + 1, TurnID: t.ID, Model: s.model, Effort: s.effort, ServiceTier: s.serviceTier, RecordedAt: at, Tokens: usage}
	// Only accept a native value explicitly scoped to last_token_usage. The
	// task_complete value is turn-wide and remains on liveTurn instead.
	if value, ok := last["time_to_first_token_ms"].(json.Number); ok {
		if ms, err := value.Float64(); err == nil && ms >= 0 {
			r.TTFT = &liveFirstToken{Seconds: ms / 1000, Source: "native_response", Note: "最近一次响应用量直接记录的原生首 Token 耗时。"}
		}
	}
	t.ServiceTier = s.serviceTier
	if len(s.serviceTiers) > 0 {
		r.ServiceTierSource, t.ServiceTierSource = "logged", "logged"
	}
	if s.start != nil {
		r.StartedAt = *s.start
	}
	if s.generatedEnd != nil {
		r.CompletedAt = *s.generatedEnd
	}
	if s.firstGenerated != nil {
		r.FirstOutputAt = *s.firstGenerated
	}
	if s.sawStart && s.start != nil && s.generatedEnd != nil {
		elapsed := s.generatedEnd.Sub(*s.start).Seconds()
		if elapsed > 0 {
			// Preserve upstream's effective output TPS semantics through ResponseSample.
			sample := ResponseSample{ElapsedSeconds: elapsed, OutputTokens: usage.Output, ReasoningOutputTokens: usage.Reasoning, VisibleOutputTokens: usage.Visible}
			r.Seconds = elapsed
			output, visible, reasoning := sample.EffectiveTPS(), sample.VisibleTPS(), sample.ReasoningTPS()
			r.TPS = liveRates{Output: &output, Visible: &visible, Reasoning: &reasoning, Perceived: &output}
		}
	}
	if r.Seconds == 0 {
		r.TimingNote = "缺少可靠响应边界；保留 token，TPS 不可用"
		s.warn("存在无法计时的响应；TPS 仅计算有可靠时间边界的响应")
	}
	if usage.Reasoning > usage.Output || usage.Cached > usage.Input {
		s.warn("部分日志的 token 分项超过所属总项，请核对原始记录")
	}
	t.Responses = append(t.Responses, r)
	t.Metrics.add(r)
	s.Metrics.add(r)
	if at.After(s.LastResponseAt) {
		s.LastResponseAt = at
	}
	s.start, s.generatedEnd, s.firstGenerated = timePointer(at), nil, nil
}
