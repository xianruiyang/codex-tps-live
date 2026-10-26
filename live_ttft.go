package codextps

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// No exporter, proxy, Codex setting or request hook is installed. SQLite is opened
// read-only, without a busy wait or a long-running read transaction.
type liveFirstToken struct {
	Seconds   float64 `json:"seconds"`
	Source    string  `json:"source"`
	Estimated bool    `json:"estimated"`
	Assumed   bool    `json:"assumed"`
	// Retained for API/CSV compatibility; the upper-bound fallback is retired.
	UpperBound bool   `json:"upper_bound"`
	Note       string `json:"note"`
}

type liveTimingEvent struct {
	id                          int64
	at                          time.Time
	process, thread, turn, kind string
	status                      int
}

type liveTimingReader struct {
	path           string
	db             *sql.DB
	lastRead       time.Time
	latest, oldest int64
	historyFrom    time.Time
	historyDone    bool
	reset          bool
	backfillNext   bool
}

var timingThread = regexp.MustCompile(`\bthread_id[=:]\s*"?([0-9a-fA-F-]{36})`)
var timingTurn = regexp.MustCompile(`\bturn_id[=:]\s*"?([0-9a-fA-F-]{36})`)
var timingStatus = regexp.MustCompile(`\bstatus[=:]\s*(\d{3})\b`)

func timingField(pattern *regexp.Regexp, body string) string {
	if match := pattern.FindStringSubmatch(body); len(match) == 2 {
		return match[1]
	}
	return ""
}

func (r *liveTimingReader) close() {
	if r != nil && r.db != nil {
		_ = r.db.Close()
	}
}

func (r *liveTimingReader) read(now, since time.Time) ([]liveTimingEvent, error) {
	if now.Sub(r.lastRead) < time.Second {
		return nil, nil
	}
	r.lastRead = now
	if r.db == nil {
		if _, err := os.Stat(r.path); err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		absolute, err := filepath.Abs(r.path)
		if err != nil {
			return nil, err
		}
		u := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
		if filepath.VolumeName(absolute) != "" && !strings.HasPrefix(u.Path, "/") {
			u.Path = "/" + u.Path
		}
		q := u.Query()
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(1)")
		q.Add("_pragma", "busy_timeout(0)")
		u.RawQuery = q.Encode()
		r.db, err = sql.Open("sqlite", u.String())
		if err != nil {
			return nil, err
		}
		r.db.SetMaxOpenConns(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	var maximum int64
	if err := r.db.QueryRowContext(ctx, "SELECT coalesce(max(id),0) FROM logs").Scan(&maximum); err != nil {
		return nil, err
	}
	if maximum < r.latest { // A replaced/rotated database has a new ID sequence.
		r.latest, r.oldest, r.historyDone = 0, 0, false
		r.reset = true
	}
	if r.historyFrom.IsZero() || since.Before(r.historyFrom) {
		r.historyFrom, r.historyDone = since, false
	}
	const fields = `SELECT id,ts,ts_nanos,coalesce(process_uuid,''),coalesce(thread_id,''),target,substr(coalesce(feedback_log_body,''),1,8192) FROM logs WHERE target IN ('codex_http_client::request','codex_http_client::client','codex_api::endpoint::responses_websocket') AND ts >= ?`
	query, args := "", []any{since.Unix()}
	forward := r.latest > 0 && maximum > r.latest
	if forward && r.backfillNext && !r.historyDone && r.oldest > 0 {
		forward = false
	}
	if forward {
		r.backfillNext = true
		query = fields + " AND id > ? AND id <= ? ORDER BY id LIMIT 128"
		args = append(args, r.latest, maximum)
	} else if r.latest == 0 {
		query = fields + " AND id <= ? ORDER BY ts DESC,ts_nanos DESC,id DESC LIMIT 128"
		args = append(args, maximum)
	} else if !r.historyDone && r.oldest > 0 {
		query = fields + " AND id < ? ORDER BY ts DESC,ts_nanos DESC,id DESC LIMIT 128"
		args = append(args, r.oldest)
	} else {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []liveTimingEvent{}
	count, oldest, newest := 0, int64(0), int64(0)
	for rows.Next() {
		var event liveTimingEvent
		var seconds, nanos int64
		var target, body string
		if err := rows.Scan(&event.id, &seconds, &nanos, &event.process, &event.thread, &target, &body); err != nil {
			return nil, err
		}
		count++
		if oldest == 0 || event.id < oldest {
			oldest = event.id
		}
		if event.id > newest {
			newest = event.id
		}
		event.at = time.Unix(seconds, nanos)
		if event.thread == "" {
			event.thread = timingField(timingThread, body)
		}
		event.turn = timingField(timingTurn, body)
		switch {
		case target == "codex_http_client::request" && strings.Contains(body, "Compressed request body with zstd"):
			event.kind = "compressed"
		case target == "codex_http_client::client":
			status := timingField(timingStatus, body)
			event.status, _ = strconv.Atoi(status)
			if event.status != 0 {
				event.kind = "headers"
			}
		case target == "codex_api::endpoint::responses_websocket":
			event.kind = "websocket"
		}
		// Raw log bodies (including HTTP headers) never enter the API or cache.
		if event.kind != "" {
			result = append(result, event)
		}
	}
	readErr := rows.Err()
	if readErr != nil && (count == 0 || !errors.Is(readErr, context.DeadlineExceeded)) {
		return nil, readErr
	}
	if forward {
		if count < 128 && readErr == nil {
			r.latest = maximum
		} else {
			r.latest = newest
		}
	} else {
		r.backfillNext = false
		if r.latest == 0 {
			r.latest = maximum
		}
		if oldest > 0 {
			r.oldest = oldest
		}
		r.historyDone = count < 128 && readErr == nil
	}
	return result, nil
}

type liveTimingIndex struct {
	events  map[int64]liveTimingEvent
	threads map[string][]liveTimingEvent
	headers map[string][]liveTimingEvent
	pending bool
}

func (x *liveTimingIndex) add(events []liveTimingEvent) {
	if len(events) == 0 {
		return
	}
	if x.events == nil {
		x.events = map[int64]liveTimingEvent{}
	}
	for _, e := range events {
		x.events[e.id] = e
	}
	x.threads, x.headers = map[string][]liveTimingEvent{}, map[string][]liveTimingEvent{}
	for _, e := range x.events {
		if e.thread != "" {
			x.threads[e.thread] = append(x.threads[e.thread], e)
		}
		if e.kind == "headers" && e.thread == "" {
			x.headers[e.process] = append(x.headers[e.process], e)
		}
	}
	for _, groups := range []map[string][]liveTimingEvent{x.threads, x.headers} {
		for _, group := range groups {
			sort.Slice(group, func(i, j int) bool {
				if group[i].at.Equal(group[j].at) {
					return group[i].id < group[j].id
				}
				return group[i].at.Before(group[j].at)
			})
		}
	}
}

func timingRange(events []liveTimingEvent, start, end time.Time) []liveTimingEvent {
	i := sort.Search(len(events), func(i int) bool { return !events[i].at.Before(start) })
	j := sort.Search(len(events), func(i int) bool { return events[i].at.After(end) })
	return events[i:j]
}

func (m *liveMonitor) enrichTiming(s *liveSession, turn *liveTurn, r *liveResponse) {
	m.fillTransport(s, turn, r)
	r.TPS.Perceived = r.TPS.Output
	if r.TTFT != nil && !r.TTFT.Estimated && r.applyFirstToken() {
		return
	}
	if r.StartedAt.IsZero() || r.CompletedAt.IsZero() || r.Seconds <= 0 {
		r.assumeFirstOnly(turn, "响应时间边界不完整。")
		return
	}
	// Provider engine/cumulative timing is diagnostic data. It has neither a
	// client first-content timestamp nor a verified rollout response identity,
	// so it must not alter TTFT, transport attribution or generation TPS.
	var requests []liveTimingEvent
	for _, e := range timingRange(m.timings.threads[s.ID], r.StartedAt, r.CompletedAt) {
		if e.kind == "compressed" && (e.turn == "" || e.turn == turn.ID) {
			requests = append(requests, e)
		}
	}
	if len(requests) == 0 {
		r.assumeFirstOnly(turn, "未记录可归属的逐次首 Token 或 HTTP 压缩计时。")
		return
	}
	first := requests[0]
	var header *liveTimingEvent
	for _, e := range timingRange(m.timings.threads[s.ID], first.at, r.CompletedAt) {
		if e.kind == "headers" && e.status == 200 && e.process == first.process && e.turn == turn.ID {
			copy := e
			header = &copy // last successful attempt includes retry wait
		}
	}
	value := liveFirstToken{Estimated: true, Source: "request_headers", Note: "同进程、同对话、同轮次，压缩日志→HTTP 200 响应头；尚不含响应头后的首 token 等待。"}
	if header != nil {
		value.Seconds = header.at.Sub(first.at).Seconds()
		if len(requests) > 1 {
			value.Note += "包含本次响应内多次尝试的等待。"
		}
	} else {
		// Older clients can omit the header's thread span. A sole successful
		// candidate in the response bounds is usable as an explicit estimate.
		candidates := []liveTimingEvent{}
		for _, e := range timingRange(m.timings.headers[first.process], first.at, r.CompletedAt) {
			if e.status == 200 {
				candidates = append(candidates, e)
			}
		}
		if len(candidates) == 1 {
			value.Seconds = candidates[0].at.Sub(first.at).Seconds()
			value.Source, value.Note = "request_headers_temporal", "响应头缺少对话标识，使用同进程、本响应边界内唯一 HTTP 200 响应头估算。"
		} else {
			// A compression timestamp still provides an observed lower bound:
			// the first token cannot precede preparation of its own request.
			value.Seconds = first.at.Sub(r.StartedAt).Seconds()
			value.Source, value.Note = "request_preparation", "仅有压缩日志可归属：使用响应开始→请求压缩的已知等待下限；响应头缺失或未能唯一归属，未把其他请求强行配对。"
		}
	}
	if value.Seconds < 0 {
		return
	}
	r.TTFT = &value
	if !r.applyFirstToken() {
		r.assumeFirstOnly(turn, "HTTP 日志估值与本次响应时间不一致，未将其用于扣除等待。")
	}
}

// The user's provisional policy: keep attributable request timings; otherwise
// use the native turn TTFT only for its first response and assume zero later.
// The native clock starts at task launch, so wall-clock log offsets can only
// approximate its conversion to the first response's duration origin.
func (r *liveResponse) assumeFirstOnly(turn *liveTurn, reason string) {
	first := len(turn.Responses) > 0 && r.Index == turn.Responses[0].Index
	if first && turn.TimeToFirstTokenMS != nil && *turn.TimeToFirstTokenMS >= 0 && !turn.StartedAt.IsZero() && !r.StartedAt.IsZero() {
		offset := max(0, r.StartedAt.Sub(turn.StartedAt).Seconds())
		wait := max(0, float64(*turn.TimeToFirstTokenMS)/1000-offset)
		if r.Seconds > wait {
			r.TTFT = &liveFirstToken{Seconds: wait, Source: "turn_first_token", Estimated: true, Note: reason + "按每轮仅首次计入 TTFT 的约定，使用轮次原生 TTFT 并扣除任务开始至首响应开始的日志时间差近似换算；原生计时从任务启动开始，此值不是逐请求实测。"}
			r.applyFirstToken()
			return
		}
		reason += "轮次原生 TTFT 无法与首响应耗时一致换算。"
	}
	source, note := "subsequent_assumed_zero", "按每轮仅首次计入 TTFT 的临时约定，后续响应暂按 0；并非实测为零，实际 TPS 据此估算并等于可见 TPS。"
	if first {
		source, note = "first_missing_assumed_zero", "首次响应缺少可用首 Token 计时，暂按 0 估算；并非实测为零。"
		if turn.Status == "active" {
			source, note = "first_pending_assumed_zero", "首次响应的轮次原生 TTFT 尚未上报，暂按 0 估算；轮次完成并上报后自动更新。"
		}
	}
	r.TTFT = &liveFirstToken{Source: source, Estimated: true, Assumed: true, Note: reason + note}
	r.applyFirstToken()
}

func (m *liveMonitor) fillTransport(s *liveSession, turn *liveTurn, r *liveResponse) {
	r.Transport, r.TransportNote = "unknown", "现存客户端日志未记录可归属的传输方式；不通过模型名或 TTFT 数值猜测。"
	if r.StartedAt.IsZero() || r.CompletedAt.IsZero() || r.CompletedAt.Before(r.StartedAt) {
		return
	}
	var firstHTTP, firstWS time.Time
	for _, event := range timingRange(m.timings.threads[s.ID], r.StartedAt, r.CompletedAt) {
		if event.turn != "" && event.turn != turn.ID {
			continue
		}
		if (event.kind == "compressed" || event.kind == "headers" && event.status == 200) && firstHTTP.IsZero() {
			firstHTTP = event.at
		}
		if event.kind == "websocket" && firstWS.IsZero() {
			firstWS = event.at
		}
	}
	if !firstHTTP.IsZero() {
		r.Transport, r.TransportNote = "http", "同一对话、轮次及响应时间范围内有 HTTP 请求压缩或响应头日志。"
		r.TransportFallback = !firstWS.IsZero() && firstWS.Before(firstHTTP)
		if r.TransportFallback {
			r.TransportNote += "先有 WebSocket 端点记录，随后进入 HTTP 路径，按 HTTP 回退显示；不是抓包确认。"
		}
	} else if !firstWS.IsZero() {
		r.Transport, r.TransportNote = "websocket", "同一对话、轮次及响应时间范围内有 WebSocket 端点日志，未观测 HTTP 路径；按客户端日志判断，不把端点事件当作首 token。"
	} else {
		// Reused connections need not emit an endpoint event on every request.
		// Preserve the evidence level instead of pretending this is a new event.
		events := m.timings.threads[s.ID]
		before := sort.Search(len(events), func(i int) bool { return events[i].at.After(r.StartedAt) })
		for i := before - 1; i >= 0; i-- {
			e := events[i]
			if e.at.Before(turn.StartedAt) {
				break
			}
			if e.turn != turn.ID {
				continue
			}
			if e.kind == "websocket" {
				r.Transport = "websocket"
			} else if e.kind == "compressed" || e.kind == "headers" && e.status == 200 {
				r.Transport = "http"
			} else {
				continue
			}
			r.TransportInferred = true
			r.TransportNote = "本次未记录独立传输事件；沿用同一对话、同一轮次此前最近的传输日志推断（可能复用连接），不是逐请求确认。"
			break
		}
	}
}

func (r *liveResponse) applyFirstToken() bool {
	if r.TTFT.Source == "request_headers" || r.TTFT.Source == "request_headers_temporal" {
		upper := r.FirstOutputAt.Sub(r.StartedAt).Seconds()
		if !r.FirstOutputAt.IsZero() && r.TTFT.Seconds > upper+.001 {
			return false
		}
	}
	r.GenerationSeconds = r.Seconds - r.TTFT.Seconds
	if r.GenerationSeconds <= 0 {
		r.GenerationSeconds = 0
		return false
	}
	r.TPS.Actual = ratio(float64(r.Tokens.Output), r.GenerationSeconds)
	r.TPS.ActualEstimated = r.TTFT.Estimated
	r.TPS.ActualRange = false
	r.GenerationMinSeconds, r.GenerationMaxSeconds = r.GenerationSeconds, r.GenerationSeconds
	r.TPS.ActualMin = ratio(float64(r.Tokens.Output), r.GenerationMaxSeconds)
	r.TPS.ActualMax = ratio(float64(r.Tokens.Output), r.GenerationMinSeconds)
	if r.TTFT.Source == "request_preparation" {
		r.setActualBounds(0, r.GenerationSeconds)
	}
	return true
}

func (r *liveResponse) setActualBounds(minimum, maximum float64) {
	r.GenerationMinSeconds, r.GenerationMaxSeconds = max(0, minimum), maximum
	r.GenerationSeconds = 0
	r.TPS.Actual, r.TPS.ActualRange, r.TPS.ActualEstimated = nil, true, true
	r.TPS.ActualMin = ratio(float64(r.Tokens.Output), maximum)
	r.TPS.ActualMax = ratio(float64(r.Tokens.Output), minimum)
}

func (m *liveMonitor) timedTurn(s *liveSession, turn *liveTurn) liveMetrics {
	metrics := liveMetrics{}
	for _, recorded := range turn.Responses {
		r := recorded
		m.enrichTiming(s, turn, &r)
		metrics.add(r)
	}
	return metrics
}

func (m *liveMonitor) pollTiming(now time.Time) {
	since := now.AddDate(0, 0, -max(7, m.days))
	m.mu.RLock()
	if !m.trendSince.IsZero() && m.trendSince.Before(since) {
		since = m.trendSince
	}
	m.mu.RUnlock()
	if m.timingReader == nil {
		m.timingReader = &liveTimingReader{path: filepath.Join(m.home, "logs_2.sqlite")}
	}
	if now.Sub(m.timingReader.lastRead) < time.Second {
		return
	}
	events, err := m.timingReader.read(now, since)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		m.timingError = ""
	} else {
		m.timingError = "首 Token 本地日志读取暂不可用: " + err.Error()
	}
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		m.addError("首 Token 本地日志读取暂不可用: " + err.Error())
	}
	if m.timingReader.reset {
		m.timings = liveTimingIndex{}
		m.timingReader.reset = false
	}
	m.timings.add(events)
	m.timings.pending = !m.timingReader.historyDone
}
