package codextps

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"
)

// Active chart views share a time-based read request without overriding the
// conversation page's turn window. Hidden/closed/paused views expire naturally.
type liveTrendRequest struct {
	from time.Time
	seen time.Time
}

func (m *liveMonitor) updateTrendScope(now time.Time) {
	var since time.Time
	for id, view := range m.trendViews {
		if now.Sub(view.seen) > 15*time.Second {
			delete(m.trendViews, id)
			continue
		}
		if since.IsZero() || view.from.Before(since) {
			since = view.from
		}
	}
	if !since.IsZero() && (m.trendSince.IsZero() || since.Before(m.trendSince)) && m.days > 0 && since.Before(now.AddDate(0, 0, -m.days)) {
		m.discovered = time.Time{}
	}
	m.trendSince = since
}

type liveTrendPoint struct {
	At         int64   `json:"at"`
	OutputTPS  float64 `json:"output_tps"`
	VisibleTPS float64 `json:"visible_tps"`
	Output     int64   `json:"output"`
	Reasoning  int64   `json:"reasoning"`
	Seconds    float64 `json:"seconds"`
	SessionID  string  `json:"session_id"`
	Turn       int     `json:"turn"`
	Response   int     `json:"response"`
	Agent      string  `json:"agent"`
	Child      bool    `json:"child"`
	Inherited  bool    `json:"inherited"`
}

type liveTrendSeries struct {
	Model       string           `json:"model"`
	Effort      string           `json:"effort"`
	ServiceTier string           `json:"service_tier"`
	Points      []liveTrendPoint `json:"points"`
}

type liveTrendData struct {
	From         time.Time         `json:"from"`
	To           time.Time         `json:"to"`
	UpdatedAt    time.Time         `json:"updated_at"`
	PendingFiles int               `json:"pending_files"`
	Files        int               `json:"files"`
	Sessions     int               `json:"sessions"`
	Untimed      int               `json:"untimed"`
	Archived     bool              `json:"archived"`
	Errors       []string          `json:"errors"`
	Series       []liveTrendSeries `json:"series"`
}

func liveTrendRange(r *http.Request) (time.Time, time.Time, error) {
	to := time.Now()
	if raw := r.URL.Query().Get("to"); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("to must be an ISO 8601 timestamp")
		}
		to = parsed
	}
	from := to.Add(-24 * time.Hour)
	if raw := r.URL.Query().Get("from"); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("from must be an ISO 8601 timestamp")
		}
		from = parsed
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("from must be earlier than to")
	}
	return from, to, nil
}

func (m *liveMonitor) trends(from, to time.Time, view string) liveTrendData {
	m.mu.Lock()
	defer m.mu.Unlock()
	if view == "" {
		view = "default"
	}
	if m.trendViews == nil {
		m.trendViews = map[string]liveTrendRequest{}
	}
	m.trendViews[view] = liveTrendRequest{from: from, seen: time.Now()}
	m.updateTrendScope(time.Now())
	data := liveTrendData{From: from, To: to, UpdatedAt: m.updated, Files: len(m.files), Archived: true, Errors: append([]string{}, m.errors...), Series: []liveTrendSeries{}}
	if m.discovered.IsZero() {
		data.PendingFiles++
	}
	for _, f := range m.files {
		f.requestSince(m.trendSince)
		if !f.initialized || f.offset < f.size || f.rangeMissing(from) {
			data.PendingFiles++
		}
	}
	sessions := m.sessions()
	groups := map[string]*liveTrendSeries{}
	for _, file := range sessions {
		s := file.session
		inRange := false
		for _, turn := range s.Turns {
			for _, recorded := range turn.Responses {
				if recorded.RecordedAt.Before(from) || recorded.RecordedAt.After(to) {
					continue
				}
				inRange = true
				r := recorded
				if r.Seconds <= 0 || r.TPS.Output == nil || r.TPS.Visible == nil || !validLiveTrendRate(*r.TPS.Output) || !validLiveTrendRate(*r.TPS.Visible) {
					data.Untimed++
					continue
				}
				fillResponseServiceTier(s, &r, sessions)
				tier := ""
				switch r.ServiceTier {
				case "fast", "priority":
					tier = "fast"
				case "default", "ultrafast":
					tier = r.ServiceTier
				}
				model := r.Model
				if model == "" {
					model = "未记录模型"
				}
				key := model + "\x00" + r.Effort + "\x00" + tier
				group := groups[key]
				if group == nil {
					group = &liveTrendSeries{Model: model, Effort: r.Effort, ServiceTier: tier, Points: []liveTrendPoint{}}
					groups[key] = group
				}
				agent := "主对话"
				if s.ParentID != "" {
					agent = s.agentName()
					if agent == "" {
						agent = "子代理"
					}
				}
				group.Points = append(group.Points, liveTrendPoint{At: r.RecordedAt.UnixMilli(), OutputTPS: *r.TPS.Output, VisibleTPS: *r.TPS.Visible, Output: r.Tokens.Output, Reasoning: r.Tokens.Reasoning, Seconds: r.Seconds, SessionID: s.ID, Turn: turn.Number, Response: r.TurnIndex, Agent: agent, Child: s.ParentID != "", Inherited: r.ServiceTierSource == "inherited"})
			}
		}
		if inRange {
			data.Sessions++
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		group := groups[key]
		sort.Slice(group.Points, func(i, j int) bool {
			a, b := group.Points[i], group.Points[j]
			if a.At != b.At {
				return a.At < b.At
			}
			if a.SessionID != b.SessionID {
				return a.SessionID < b.SessionID
			}
			if a.Turn != b.Turn {
				return a.Turn < b.Turn
			}
			return a.Response < b.Response
		})
		data.Series = append(data.Series, *group)
	}
	return data
}

func validLiveTrendRate(rate float64) bool {
	return rate >= 0 && !math.IsNaN(rate) && !math.IsInf(rate, 0)
}
