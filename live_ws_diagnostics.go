package codextps

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Kept separately from the typed protocol cache: only a bounded, redacted
// diagnostic message is retained. Raw log bodies and HTTP headers stay private.
type liveWSDiagnostic struct {
	ID           string    `json:"id"`
	At           time.Time `json:"at"`
	Level        string    `json:"level"`
	Kind         string    `json:"kind"`
	Reason       string    `json:"reason"`
	Message      string    `json:"message"`
	Target       string    `json:"target"`
	Thread       string    `json:"thread_id,omitempty"`
	Turn         string    `json:"turn_id,omitempty"`
	Process      string    `json:"process_id,omitempty"`
	Title        string    `json:"title,omitempty"`
	Status       int       `json:"status,omitempty"`
	CloseCode    int       `json:"close_code,omitempty"`
	Transport    string    `json:"transport"`
	Operation    string    `json:"operation,omitempty"`
	Retries      *int      `json:"retries,omitempty"`
	MaxRetries   *int      `json:"max_retries,omitempty"`
	RetryDelayed bool      `json:"retry_delayed,omitempty"`
	SocketCode   int       `json:"socket_code,omitempty"`
	CloseOrigin  string    `json:"close_origin,omitempty"`
	Truncated    bool      `json:"truncated,omitempty"`
}

var wsDiagnosticURL = regexp.MustCompile(`(?i)\b(?:wss?|https?)://[^\s"'<>]+`)
var wsDiagnosticSecret = regexp.MustCompile(`(?i)\b(?:Bearer\s+[^\s,"'}]+|(?:sk|sess)-[A-Za-z0-9_-]{8,})`)
var wsDiagnosticSecretField = regexp.MustCompile(`(?i)(["']?\b(?:authorization|proxy-authorization|cookie|set-cookie|api[-_]key|access[-_]token|refresh[-_]token|password|secret)\b["']?\s*[=:]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;}]+)`)
var wsDiagnosticPrivatePayload = regexp.MustCompile(`(?i)["']?\b(?:headers|request_body|response_body|body|payload|input)\b["']?\s*[=:]`)
var wsDiagnosticHTTP = regexp.MustCompile(`(?i)\b(?:status(?:\s+code)?|HTTP(?:\s+error)?)[=: \t]+(\d{3})\b`)
var wsDiagnosticClose = regexp.MustCompile(`(?i)\bclose(?:[_ ]code)?[=: \t]+([1-4]\d{3})\b`)
var wsDiagnosticRetries = regexp.MustCompile(`(?i)\bretries["']?\s*[=:]\s*(\d+)\b`)
var wsDiagnosticMaxRetries = regexp.MustCompile(`(?i)\bmax_retries["']?\s*[=:]\s*(\d+)\b`)
var wsDiagnosticSocket = regexp.MustCompile(`(?i)\bos error\s+(\d+)\b`)

func wsDiagnosticInt(pattern *regexp.Regexp, body string) *int {
	match := pattern.FindStringSubmatch(body)
	if len(match) != 2 {
		return nil
	}
	value, err := strconv.Atoi(match[1])
	if err != nil {
		return nil
	}
	return &value
}

func wsContainsAny(value string, terms ...string) bool {
	for _, term := range terms {
		if strings.Contains(value, term) {
			return true
		}
	}
	return false
}

func redactedWSMessage(body string) (string, bool) {
	// Select the diagnostic template rather than the preceding span metadata.
	lower := strings.ToLower(body)
	start := -1
	for _, marker := range []string{"failed to connect to websocket", "falling back to http", "websocket error", "websocket stream", "websocket connection", "connection closed", "stream disconnected", "retrying"} {
		if index := strings.Index(lower, marker); index >= 0 && (start < 0 || index < start) {
			start = index
		}
	}
	if start >= 0 {
		body = body[start:]
	}
	// Request/response bodies, header maps and payloads can contain private data.
	if index := wsDiagnosticPrivatePayload.FindStringIndex(body); index != nil {
		body = body[:index[0]] + " [后续请求/响应字段已省略]"
	}
	body = wsDiagnosticURL.ReplaceAllStringFunc(body, func(raw string) string {
		u, err := url.Parse(strings.TrimRight(raw, ",;)}]"))
		if err != nil || u.Host == "" {
			return "[地址已脱敏]"
		}
		return u.Scheme + "://" + u.Host + "/[路径与参数已省略]"
	})
	body = wsDiagnosticSecretField.ReplaceAllString(body, "${1}[已脱敏]")
	body = wsDiagnosticSecret.ReplaceAllString(body, "[已脱敏]")
	body = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, body))
	runes := []rune(body)
	if len(runes) > 2000 {
		return string(runes[:2000]) + "…", true
	}
	return body, false
}

func parseWSDiagnostic(event liveTimingEvent, target, level, body string) *liveWSDiagnostic {
	retrySource := target == "codex_core::responses_retry"
	if !retrySource && target != "codex_api::endpoint::responses_websocket" && target != "codex_core::client" && target != "codex_core::codex" && target != "codex_api::sse::responses" {
		return nil
	}
	lower := strings.ToLower(body)
	level = strings.ToUpper(level)
	fallback := target == "codex_core::client" && strings.Contains(lower, "falling back to http")
	ws := target == "codex_api::endpoint::responses_websocket" || strings.Contains(lower, "websocket")
	if !fallback && !ws && !retrySource {
		return nil
	}
	if !fallback && level != "WARN" && level != "ERROR" && !wsContainsAny(lower, "retrying", "retry attempt", "reconnecting") {
		return nil
	}
	row := liveWSDiagnostic{At: event.at.UTC(), Level: level, Target: target, Thread: event.thread, Turn: event.turn, Process: event.process, Kind: "other", Reason: "请求传输异常", Transport: "unknown"}
	if ws {
		row.Transport = "websocket"
	}
	if fallback {
		row.Transport = "websocket_to_http"
	}
	if value := wsDiagnosticSocket.FindStringSubmatch(body); len(value) == 2 {
		row.SocketCode, _ = strconv.Atoi(value[1])
	}
	switch {
	case fallback:
		row.Kind, row.Reason = "http_fallback", "WebSocket 已回退到 HTTP"
	case strings.Contains(lower, "websocket closed by server before response.completed"):
		row.Kind, row.Reason, row.CloseOrigin = "disconnected", "服务端在响应完成前关闭 WebSocket", "server"
	case row.SocketCode != 0:
		row.Kind, row.Reason = "disconnected", fmt.Sprintf("连接中断（系统 socket 错误 %d）", row.SocketCode)
	case wsContainsAny(lower, "tls handshake eof", "tls handshake", "certificate", "rustls", "ssl error"):
		row.Kind, row.Reason = "tls", "TLS 握手或证书错误"
		if strings.Contains(lower, "tls handshake eof") {
			row.Reason = "TLS 握手时连接结束（EOF）"
		}
	case wsContainsAny(lower, "timed out", "timeout", "deadline exceeded"):
		row.Kind, row.Reason = "timeout", "连接或接收超时"
	case wsContainsAny(lower, "retrying", "retry attempt", "reconnecting"):
		row.Kind, row.Reason = "retry", "请求正在重试或重连"
	case wsContainsAny(lower, "connection closed", "stream disconnected", "connection reset", "closed by server", "connectionclosed"):
		row.Kind, row.Reason = "disconnected", "响应流连接断开"
	case strings.Contains(lower, "failed to connect"):
		row.Kind, row.Reason = "connect", "连接建立失败"
	}
	if retrySource {
		// Retry logs cover both sampling and compaction, over multiple transports.
		// Neither the target nor a nearby handshake proves this attempt used WS.
		row.Kind = "retry"
		row.Retries = wsDiagnosticInt(wsDiagnosticRetries, body)
		row.MaxRetries = wsDiagnosticInt(wsDiagnosticMaxRetries, body)
		row.RetryDelayed = strings.Contains(lower, "retrying request after delay")
		if strings.Contains(lower, "sampling_error=") {
			row.Operation = "sampling"
		}
		if strings.Contains(lower, "compact_error=") {
			row.Operation = "compact"
		}
	}
	if match := wsDiagnosticHTTP.FindStringSubmatch(body); len(match) == 2 {
		fmt.Sscan(match[1], &row.Status)
	}
	if match := wsDiagnosticClose.FindStringSubmatch(body); len(match) == 2 {
		fmt.Sscan(match[1], &row.CloseCode)
	}
	row.Message, row.Truncated = redactedWSMessage(body)
	if len(body) >= 8192 {
		row.Truncated = true
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s", event.process, event.id, event.at.UTC().Format(time.RFC3339Nano))))
	row.ID = fmt.Sprintf("%x", sum[:12])
	return &row
}

func (m *liveMonitor) addWSDiagnostics(events []liveTimingEvent) {
	for _, event := range events {
		if event.diagnostic == nil {
			continue
		}
		if m.wsDiagnostics == nil {
			m.wsDiagnostics = map[string]liveWSDiagnostic{}
		}
		m.wsDiagnostics[event.diagnostic.ID] = *event.diagnostic
	}
	if len(m.wsDiagnostics) > 4000 {
		rows := make([]liveWSDiagnostic, 0, len(m.wsDiagnostics))
		for _, row := range m.wsDiagnostics {
			rows = append(rows, row)
		}
		sortWSDiagnostics(rows)
		for _, row := range rows[4000:] {
			delete(m.wsDiagnostics, row.ID)
		}
		m.wsDiagnosticsTrimmed = true
	}
}

func sortWSDiagnostics(rows []liveWSDiagnostic) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].At.Equal(rows[j].At) {
			return rows[i].ID > rows[j].ID
		}
		return rows[i].At.After(rows[j].At)
	})
}

type liveWSDiagnosticThread struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}
type liveWSDiagnosticSnapshot struct {
	Updated       time.Time                `json:"updated_at"`
	Rows          []liveWSDiagnostic       `json:"rows"`
	Threads       []liveWSDiagnosticThread `json:"threads"`
	Total         int                      `json:"total"`
	Errors        int                      `json:"errors"`
	Retries       int                      `json:"retries"`
	Fallbacks     int                      `json:"fallbacks"`
	Unconfirmed   int                      `json:"unconfirmed"`
	NextBefore    string                   `json:"next_before,omitempty"`
	CursorExpired bool                     `json:"cursor_expired"`
	Pending       bool                     `json:"pending"`
	Unavailable   bool                     `json:"unavailable"`
	ReadError     string                   `json:"read_error,omitempty"`
	Trimmed       bool                     `json:"trimmed"`
	Capture       liveCaptureStatus        `json:"capture"`
}

func (m *liveMonitor) websocketDiagnostics(from, to time.Time, kind, thread, query, before string, limit int) liveWSDiagnosticSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.wsDiagnosticSince.IsZero() || from.Before(m.wsDiagnosticSince) {
		m.wsDiagnosticSince = from
	}
	result := liveWSDiagnosticSnapshot{Updated: m.updated, Rows: []liveWSDiagnostic{}, Threads: []liveWSDiagnosticThread{}, Pending: m.timings.pending, Unavailable: m.timings.unavailable, Trimmed: m.wsDiagnosticsTrimmed, Capture: m.captureStatus}
	if m.timingError != "" {
		result.ReadError = strings.TrimPrefix(m.timingError, "首 Token 本地日志读取暂不可用: ")
	}
	if limit < 1 {
		limit = 100
	}
	sessions := m.sessions()
	rows := []liveWSDiagnostic{}
	threads := map[string]string{}
	query = strings.ToLower(strings.TrimSpace(query))
	for _, row := range m.wsDiagnostics {
		if row.At.Before(from) || row.At.After(to) {
			continue
		}
		row.Title = m.names.entries[row.Thread].Name
		if file := sessions[row.Thread]; file != nil && row.Title == "" {
			row.Title = file.session.Title
			if row.Title == "" {
				row.Title = file.session.AgentNickname
			}
		}
		if row.Thread != "" {
			threads[row.Thread] = row.Title
		}
		if kind != "" && row.Kind != kind {
			continue
		}
		if thread != "" && row.Thread != thread {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(row.Title+" "+row.Thread+" "+row.Reason+" "+row.Message+" "+row.Target+" "+row.Operation+" "+row.Transport), query) {
			continue
		}
		rows = append(rows, row)
		if row.Transport == "unknown" {
			result.Unconfirmed++
		}
		if row.Kind == "http_fallback" {
			result.Fallbacks++
		} else if row.Kind == "retry" {
			result.Retries++
		} else {
			result.Errors++
		}
	}
	for id, title := range threads {
		result.Threads = append(result.Threads, liveWSDiagnosticThread{id, title})
	}
	sort.Slice(result.Threads, func(i, j int) bool {
		a, b := result.Threads[i], result.Threads[j]
		if a.Title == b.Title {
			return a.ID < b.ID
		}
		return a.Title < b.Title
	})
	sortWSDiagnostics(rows)
	result.Total = len(rows)
	start := 0
	if before != "" {
		start = -1
		for index, row := range rows {
			if row.ID == before {
				start = index + 1
				break
			}
		}
		if start < 0 {
			result.CursorExpired = true
			return result
		}
	}
	end := min(start+limit, len(rows))
	result.Rows = append(result.Rows, rows[start:end]...)
	if end < len(rows) && end > start {
		result.NextBefore = rows[end-1].ID
	}
	return result
}
