package codextps

import "time"

// Counts refer to recorded model responses, not TCP connections or warmups.
// A successful HTTP fallback is counted as HTTP; a turn is mixed when its
// response paths include both protocols, or a response has conflicting paths.
type liveTransportSummary struct {
	Kind      string `json:"kind"`
	HTTP      int    `json:"http"`
	WebSocket int    `json:"websocket"`
	Mixed     int    `json:"mixed"`
	Unknown   int    `json:"unknown"`
	Inferred  int    `json:"inferred"`
	Fallback  int    `json:"fallback"`
	Pending   int    `json:"pending"`
}

func (s *liveTransportSummary) updateKind() {
	switch {
	case s.Mixed > 0 || s.HTTP > 0 && s.WebSocket > 0:
		s.Kind = "mixed"
	case s.HTTP > 0:
		s.Kind = "http"
	case s.WebSocket > 0:
		s.Kind = "websocket"
	case s.Unknown > 0:
		s.Kind = "unknown"
	default:
		s.Kind = "none"
	}
}

func (s *liveTransportSummary) add(r liveResponse) {
	switch r.Transport {
	case "http":
		s.HTTP++
	case "websocket":
		s.WebSocket++
	case "mixed":
		s.Mixed++
	default:
		s.Unknown++
		if r.TransportSource == "history_pending" {
			s.Pending++
		}
	}
	if r.TransportInferred && r.Transport != "unknown" && r.Transport != "" {
		s.Inferred++
	}
	if r.TransportFallback {
		s.Fallback++
	}
	s.updateKind()
}

func (s *liveTransportSummary) merge(other liveTransportSummary) {
	s.HTTP += other.HTTP
	s.WebSocket += other.WebSocket
	s.Mixed += other.Mixed
	s.Unknown += other.Unknown
	s.Inferred += other.Inferred
	s.Fallback += other.Fallback
	s.Pending += other.Pending
	s.updateKind()
}

func (m *liveMonitor) fillTransport(s *liveSession, turn *liveTurn, r *liveResponse) {
	r.Transport, r.TransportSource = "unknown", "evidence_missing"
	r.TransportFallback, r.TransportInferred = false, false
	r.TransportNote = "已读取的本地协议日志中未找到可归属的请求证据；不按模型、速度档位或其他对话的协议猜测。"
	if r.StartedAt.IsZero() || r.CompletedAt.IsZero() || r.CompletedAt.Before(r.StartedAt) {
		r.TransportSource, r.TransportNote = "boundary_missing", "响应的时间边界不完整，不能把邻近其他请求的协议直接归入本次。"
		if m.inferTurnTransport(s, turn, r) {
			r.TransportNote += "本次缺少完整时间边界，只推断协议，不因此补造响应耗时或 TPS。"
		}
		return
	}
	before := timingRange(m.timings.threads[s.ID], turn.StartedAt, r.StartedAt)
	priorFallback := false
	for _, e := range before {
		if e.turn != turn.ID {
			continue
		}
		if e.kind == "http_fallback" {
			priorFallback = true
		} else if e.kind == "websocket_request" || e.kind == "mixed_request" {
			// New explicit WS evidence is not overridden by older client state.
			priorFallback = false
		}
	}
	var lastHTTP, lastWS time.Time
	lastHTTPCached, lastWSCached := false, false
	cachedEvidence := false
	defer func() {
		if cachedEvidence && r.Transport != "unknown" {
			r.TransportSource += "_cache"
			r.TransportNote += "协议证据来自此前只读观察后保存在本机的最小协议索引。"
		}
	}()
	fallback, genericHTTP, connection := false, false, false
	for _, e := range timingRange(m.timings.threads[s.ID], r.StartedAt, r.CompletedAt) {
		if e.turn != "" && e.turn != turn.ID {
			continue
		}
		switch e.kind {
		case "http_request":
			lastHTTP = e.at
			lastHTTPCached = e.cached
		case "websocket_request":
			lastWS = e.at
			lastWSCached = e.cached
		case "mixed_request":
			lastHTTP, lastWS = e.at, e.at
			lastHTTPCached, lastWSCached = e.cached, e.cached
		case "http_fallback":
			fallback, lastHTTP = true, e.at
			lastHTTPCached = e.cached
		case "compressed":
			genericHTTP = true
		case "headers":
			genericHTTP = genericHTTP || e.status == 200
		case "websocket_connection":
			connection = true
		}
	}
	switch {
	case !lastHTTP.IsZero() && !lastWS.IsZero():
		cachedEvidence = lastHTTPCached && lastWSCached
		if fallback && !lastHTTP.Before(lastWS) {
			r.Transport, r.TransportSource, r.TransportFallback = "http", "http_fallback", true
			r.TransportNote = "同次响应边界内有 WebSocket 请求 span、原生 HTTP 回退和后续 HTTP 路径；按回退后的 HTTP 显示。"
		} else {
			r.Transport, r.TransportSource = "mixed", "mixed_request_spans"
			r.TransportNote = "同次响应边界内包含 HTTP 与 WebSocket 请求 span，未找到足以唯一确定最终承载路径的回退顺序；明确保留混合证据。"
		}
		return
	case !lastHTTP.IsZero():
		cachedEvidence = lastHTTPCached
		r.Transport, r.TransportSource, r.TransportFallback = "http", "http_request_span", fallback || priorFallback
		r.TransportNote = "同对话、同轮次及本次响应时间边界内的原生 HTTP 请求 span。"
		if fallback {
			r.TransportSource = "http_fallback"
			r.TransportNote = "同对话、同轮次及本次响应边界内记录了原生 WebSocket→HTTP 回退；按 HTTP 路径显示。"
		} else if priorFallback {
			r.TransportNote += "本轮此前已记录原生 WebSocket→HTTP 回退。"
		}
		return
	case !lastWS.IsZero():
		cachedEvidence = lastWSCached
		r.Transport, r.TransportSource = "websocket", "websocket_request_span"
		r.TransportNote = "同对话、同轮次及本次响应边界内的原生 WebSocket 请求 span；已排除明确 warmup/generate=false，不以单纯连接建立或共享 SSE 解析器名称判定。"
		return
	case genericHTTP:
		r.Transport, r.TransportSource, r.TransportInferred, r.TransportFallback = "http", "http_log_window", true, priorFallback
		r.TransportNote = "同对话及本次响应边界内有 HTTP 请求压缩或成功响应头；缺少独立 Responses 请求 span，按客户端 HTTP 日志推断。"
		if priorFallback {
			r.TransportNote += "本轮此前已记录原生 WebSocket→HTTP 回退。"
		}
		return
	}
	// Carry only an earlier request path in this same turn. Connection attempts
	// and events from other conversations/clients are never carried as proof.
	for i := len(before) - 1; i >= 0; i-- {
		e := before[i]
		if e.turn != turn.ID {
			continue
		}
		switch e.kind {
		case "http_request", "http_fallback":
			r.Transport = "http"
		case "websocket_request":
			r.Transport = "websocket"
		case "mixed_request":
			r.TransportSource, r.TransportNote = "conflicting_prior_path", "同轮先前请求含混合协议证据，不能作为本次连接复用的唯一依据。"
			return
		default:
			continue
		}
		r.TransportSource, r.TransportInferred = "turn_reuse", true
		cachedEvidence = e.cached
		r.TransportNote = "本次缺少独立请求 span；按同一对话、同一轮次此前最近的请求协议沿用推断，不跨对话或按父代理协议填充。"
		if r.Transport == "http" && priorFallback {
			r.TransportFallback, r.TransportSource = true, "turn_fallback_reuse"
			r.TransportNote += "此前已发生原生 WebSocket→HTTP 回退，明确保留回退沿用来源。"
		}
		return
	}
	if m.inferTurnTransport(s, turn, r) {
		return
	}

	switch {
	case m.timings.unavailable:
		r.TransportSource, r.TransportNote = "log_unavailable", "当前 Codex 数据目录没有 logs_2.sqlite；保留协议待确认，不把缺日志当作 HTTP 或 WebSocket。"
	case m.timingError != "":
		r.TransportSource, r.TransportNote = "read_error", m.timingError
	case m.timings.pending:
		r.TransportSource, r.TransportNote = "history_pending", "本地协议日志仍在补读；读取到请求 span 后会自动更新，不是已确认缺少记录。"
	case connection:
		r.TransportSource, r.TransportNote = "connection_only", "只发现 WebSocket 连接/握手日志，尚无本次请求的协议证据；预热或连接成功不代表模型请求最终使用 WebSocket。"
	default:
		events := m.timings.threads[s.ID]
		if r.TransportSource == "evidence_missing" && len(events) > 0 && r.CompletedAt.Before(events[0].at) {
			r.TransportSource, r.TransportNote = "history_gap", "本次响应早于当前索引中保留的该对话协议日志，缓存中也没有独立请求证据；旧日志可能已被清理或当时未记录，无法精确补回。"
		}
	}
}

func (m *liveMonitor) inferTurnTransport(s *liveSession, turn *liveTurn, r *liveResponse) bool {
	// A missing earlier row can use the turn's uniform typed request evidence,
	// but is explicitly an inference, not reconstructed per-request telemetry.
	end := turn.EndedAt
	if end.IsZero() {
		end = s.UpdatedAt
	}
	seenHTTP, seenWS, onlyCached, turnFallback := false, false, true, false
	for _, e := range timingRange(m.timings.threads[s.ID], turn.StartedAt, end) {
		if e.turn != turn.ID {
			continue
		}
		switch e.kind {
		case "http_request":
			seenHTTP = true
		case "http_fallback":
			seenHTTP, turnFallback = true, true
		case "websocket_request":
			seenWS = true
		case "mixed_request":
			seenHTTP, seenWS = true, true
		default:
			continue
		}
		onlyCached = onlyCached && e.cached
	}
	if seenHTTP != seenWS && !turnFallback {
		r.Transport = "http"
		if seenWS {
			r.Transport = "websocket"
		}
		r.TransportSource, r.TransportInferred = "turn_consensus", true
		r.TransportFallback = false
		r.TransportNote = "本次缺少独立请求协议记录；本轮其余已读取的原生请求 span 均为同一种协议，按同轮一致路径推断。不是本次逐请求确认；后续出现独立或冲突证据时会更新。"
		if onlyCached {
			r.TransportSource += "_cache"
			r.TransportNote += "协议证据来自此前只读观察后保存在本机的最小协议索引。"
		}
		return true
	}
	if seenHTTP && seenWS {
		r.TransportSource, r.TransportNote = "turn_mixed_unresolved", "本轮确有 HTTP 与 WebSocket 请求，但缺少能归属本次的独立协议记录，不能把本轮混合状态当成本次实际协议。"
	} else if turnFallback {
		r.TransportSource, r.TransportNote = "turn_fallback_unresolved", "本轮发生过 WebSocket→HTTP 回退，但缺少本次独立请求记录，不能把回退后的 HTTP 状态倒推到此前响应。"
	}
	return false
}
