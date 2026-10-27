package codextps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type liveWSProxyConnection struct {
	ID      string   `json:"id"`
	Host    string   `json:"host"`
	Process string   `json:"process"`
	Network string   `json:"network"`
	Chains  []string `json:"chains"`
}
type liveWSProxyEvent struct {
	ID          string    `json:"id"`
	At          time.Time `json:"at"`
	Level       string    `json:"level"`
	Message     string    `json:"message"`
	Host        string    `json:"host"`
	Process     string    `json:"process,omitempty"`
	Association string    `json:"association"`
}
type liveWSProxySnapshot struct {
	Enabled      bool                    `json:"enabled"`
	Running      bool                    `json:"running"`
	Message      string                  `json:"message"`
	Error        string                  `json:"error,omitempty"`
	UpdatedAt    time.Time               `json:"updated_at"`
	Source       string                  `json:"source"`
	Connections  []liveWSProxyConnection `json:"connections"`
	Events       []liveWSProxyEvent      `json:"events"`
	Received     uint64                  `json:"received"`
	Matched      uint64                  `json:"matched"`
	Unrecognized uint64                  `json:"unrecognized"`
}
type liveWSProxyService struct {
	mu       sync.Mutex
	home     string
	snapshot liveWSProxySnapshot
	cancel   context.CancelFunc
	done     chan struct{}
	closed   bool
	runs     sync.WaitGroup
}

func newLiveWSProxyService(homeOverride string) *liveWSProxyService {
	return &liveWSProxyService{home: homeOverride, snapshot: liveWSProxySnapshot{Message: "旁路订阅未开启；仅观察本机 Mihomo 已有日志。", UpdatedAt: time.Now(), Connections: []liveWSProxyConnection{}, Events: []liveWSProxyEvent{}}}
}
func (s *liveWSProxyService) register(mux *http.ServeMux) {
	mux.HandleFunc("/api/ws-proxy", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.copySnapshot())
	})
	mux.HandleFunc("/api/ws-proxy/control", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		d := json.NewDecoder(io.LimitReader(r.Body, 1024))
		if d.Decode(&body) != nil || body.Enabled == nil {
			http.Error(w, "需要 enabled 布尔值", 400)
			return
		}
		s.setEnabled(*body.Enabled)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.copySnapshot())
	})
}
func (s *liveWSProxyService) copySnapshot() liveWSProxySnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.snapshot
	out.Events = append([]liveWSProxyEvent{}, out.Events...)
	out.Connections = append([]liveWSProxyConnection{}, out.Connections...)
	for i := range out.Connections {
		out.Connections[i].Chains = append([]string{}, out.Connections[i].Chains...)
	}
	return out
}
func (s *liveWSProxyService) setEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.snapshot.Enabled == enabled {
		return
	}
	s.snapshot.Enabled = enabled
	s.snapshot.UpdatedAt = time.Now()
	if !enabled {
		if s.cancel != nil {
			s.cancel()
		}
		s.snapshot.Running = false
		s.snapshot.Message = "旁路订阅已关闭；历史事件仅保留于本次进程内。"
		return
	}
	s.snapshot.Running = false
	s.snapshot.Message = "正在连接本机 Mihomo 既有告警流…"
	s.snapshot.Error = ""
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	previous := s.done
	done := make(chan struct{})
	s.done = done
	s.runs.Add(1)
	go func() {
		defer s.runs.Done()
		defer close(done)
		if previous != nil {
			// Preserve the entire predecessor chain even when this generation
			// was canceled before it started, so rapid toggles cannot overlap readers.
			<-previous
		}
		if ctx.Err() != nil {
			return
		}
		s.run(ctx)
	}()
}
func (s *liveWSProxyService) close() {
	s.mu.Lock()
	s.closed = true
	s.snapshot.Enabled = false
	s.snapshot.Running = false
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.runs.Wait()
}
func (s *liveWSProxyService) status(ctx context.Context, running bool, source, msg, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	s.snapshot.Running = running
	s.snapshot.Source = source
	s.snapshot.Message = msg
	s.snapshot.Error = errText
	s.snapshot.UpdatedAt = time.Now()
}

type liveWSProxyController struct {
	address, secret, source string
	dial                    func(context.Context, string, string) (net.Conn, error)
}

func discoverLiveWSProxyController(home string) (liveWSProxyController, error) {
	home = resolveLiveClashHome(home)
	if home == "" {
		return liveWSProxyController{}, errors.New("未发现 Clash Verge 配置目录")
	}
	var cfg struct {
		TCP    string `yaml:"external-controller"`
		Pipe   string `yaml:"external-controller-pipe"`
		Secret string `yaml:"secret"`
	}
	if err := readLiveNodeYAML(filepath.Join(home, "config.yaml"), &cfg); err != nil {
		return liveWSProxyController{}, errors.New("无法读取本地 Mihomo 控制器配置")
	}
	if c, ok := liveWSProxyPipeController(cfg.Pipe, cfg.Secret); ok {
		return c, nil
	}
	host, port, err := net.SplitHostPort(cfg.TCP)
	if err != nil || port == "" {
		return liveWSProxyController{}, errors.New("未发现可用的本机正式控制管道或 loopback 控制器")
	}
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return liveWSProxyController{}, errors.New("控制器不是明确的 loopback 地址，未连接")
	}
	d := &net.Dialer{Timeout: 4 * time.Second}
	return liveWSProxyController{address: net.JoinHostPort(host, port), secret: cfg.Secret, source: "Mihomo本机TCP", dial: d.DialContext}, nil
}
func (s *liveWSProxyService) run(ctx context.Context) {
	delay := time.Second
	for ctx.Err() == nil {
		c, err := discoverLiveWSProxyController(s.home)
		if err == nil {
			err = s.observe(ctx, c)
		}
		if ctx.Err() != nil {
			return
		}
		msg := "订阅中断；将缓退避重连。已有日志可能缺少目标或进程字段，不能据空事件判断无错误。"
		errText := proxyControllerError(err)
		if c.source == "" {
			errText = err.Error()
		}
		s.status(ctx, false, c.source, msg, errText)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}
func (s *liveWSProxyService) observe(ctx context.Context, c liveWSProxyController) error {
	transport := &http.Transport{DialContext: c.dial, Proxy: nil, ResponseHeaderTimeout: 4 * time.Second, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	headers := http.Header{}
	if c.secret != "" {
		headers.Set("Authorization", "Bearer "+c.secret)
	}
	dialer := websocket.Dialer{NetDialContext: c.dial, HandshakeTimeout: 5 * time.Second, Proxy: nil}
	ws, response, err := dialer.DialContext(ctx, "ws://"+c.address+"/logs?level=warning&format=structured", headers)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		if response != nil {
			return proxyHTTPError(response.StatusCode)
		}
		return err
	}
	defer ws.Close()
	ws.SetReadLimit(64 << 10)
	// A warning stream can be quiet indefinitely. Cancellation closes our local
	// pipe; missing log traffic or heartbeat replies are not stream errors.
	child, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); ws.Close(); workers.Wait() }()
	workers.Add(1)
	go func() { defer workers.Done(); <-child.Done(); ws.Close() }()
	s.status(ctx, true, c.source, "已订阅现有 warning/error；仅显示明确 OpenAI/ChatGPT 目标。日志字段缺失会限制覆盖，未关联具体 Codex 请求。", "")
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			err := s.pollConnections(child, client, c, headers)
			detail := ""
			if err != nil {
				detail = "连接快照读取失败：" + proxyControllerError(err)
			}
			s.status(child, true, c.source, "已订阅现有 warning/error；仅显示明确 OpenAI/ChatGPT 目标。日志字段缺失会限制覆盖，未关联具体 Codex 请求。", detail)
			timer := time.NewTimer(10 * time.Second)
			select {
			case <-child.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	for {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.acceptEvent(raw)
	}
}
func (s *liveWSProxyService) pollConnections(ctx context.Context, client *http.Client, c liveWSProxyController, headers http.Header) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+c.address+"/connections", nil)
	req.Header = headers.Clone()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return proxyHTTPError(resp.StatusCode)
	}
	var body struct {
		Connections []struct {
			ID       string `json:"id"`
			Metadata struct {
				Host        string `json:"host"`
				Process     string `json:"process"`
				ProcessPath string `json:"processPath"`
				Network     string `json:"network"`
			} `json:"metadata"`
			Chains []string `json:"chains"`
		} `json:"connections"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body) != nil {
		return errors.New("connections 无法解析")
	}
	out := []liveWSProxyConnection{}
	for _, v := range body.Connections {
		process := proxyProcess(v.Metadata.Process)
		if process == "" {
			process = proxyProcess(v.Metadata.ProcessPath)
		}
		host := proxyTarget(v.Metadata.Host)
		if host == "" || !proxyCodexProcess(process) {
			continue
		}
		chains := []string{}
		for _, chain := range v.Chains {
			if len(chains) >= 16 {
				break
			}
			chains = append(chains, proxySafeText(chain, 200))
		}
		out = append(out, liveWSProxyConnection{proxyHash(v.ID), host, process, proxySafeText(v.Metadata.Network, 30), chains})
		if len(out) >= 100 {
			break
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() == nil {
		s.snapshot.Connections = out
		s.snapshot.UpdatedAt = time.Now()
	}
	return nil
}

type proxyHTTPError int

func (e proxyHTTPError) Error() string { return fmt.Sprintf("本机控制器 HTTP %d", int(e)) }
func proxyControllerError(err error) string {
	var status proxyHTTPError
	if errors.As(err, &status) {
		return status.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "本机控制器连接超时"
	}
	var n net.Error
	if errors.As(err, &n) && n.Timeout() {
		return "本机控制器连接或读取超时"
	}
	if errors.Is(err, os.ErrPermission) {
		return "本机控制器访问被拒绝（权限不足）"
	}
	var closed *websocket.CloseError
	if errors.As(err, &closed) {
		return fmt.Sprintf("本机控制器 WebSocket 已关闭（代码 %d）", closed.Code)
	}
	if err != nil {
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "refused") || strings.Contains(lower, "actively refused") {
			return "本机控制器拒绝连接"
		}
		if strings.Contains(lower, "access is denied") || strings.Contains(lower, "permission denied") {
			return "本机控制器访问被拒绝（权限不足）"
		}
		if strings.Contains(lower, "connections 无法解析") {
			return "本机控制器连接快照格式无法解析"
		}
	}
	return "本机控制器传输或握手失败"
}
func proxyHash(v string) string { sum := sha256.Sum256([]byte(v)); return hex.EncodeToString(sum[:8]) }
func proxyProcess(v string) string {
	v = strings.ReplaceAll(v, "\\", "/")
	p := filepath.Base(v)
	if p == "." || p == "/" {
		return ""
	}
	return p
}
func proxyCodexProcess(v string) bool {
	return strings.EqualFold(v, "codex.exe") || strings.EqualFold(v, "ChatGPT.exe")
}
func proxyTarget(v string) string {
	v = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(v), "."))
	if h, _, err := net.SplitHostPort(v); err == nil {
		v = h
	}
	for _, domain := range []string{"openai.com", "chatgpt.com"} {
		if v == domain || strings.HasSuffix(v, "."+domain) {
			return v
		}
	}
	return ""
}

var proxyDomainRE = regexp.MustCompile(`(?i)\b[a-z0-9-]+(?:\.[a-z0-9-]+)+\b`)
var proxyProcessRE = regexp.MustCompile(`(?i)\bprocess(?:name)?[=: ]+["']?([^\s,"';)]+)`)
var proxyURLRE = regexp.MustCompile(`(?i)(?:https?|wss?|socks5?)://[^\s"'<>]+`)
var proxyCredentialRE = regexp.MustCompile(`(?i)(?:authorization|bearer|secret|password|token|api[-_]?key)\s*[:= ]\s*[^\s,;]+`)
var proxyIPRE = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?::\d+)?\b|\[[0-9a-fA-F:]+\](?::\d+)?`)

func proxySafeText(v string, limit int) string {
	v = proxyURLRE.ReplaceAllStringFunc(v, func(raw string) string {
		u, e := url.Parse(raw)
		if e == nil {
			if host := proxyTarget(u.Hostname()); host != "" {
				return host + "/[路径已隐藏]"
			}
		}
		return "[地址已隐藏]"
	})
	v = proxyCredentialRE.ReplaceAllString(v, "[凭据已隐藏]")
	v = proxyIPRE.ReplaceAllString(v, "[地址已隐藏]")
	v = proxyDomainRE.ReplaceAllStringFunc(v, func(host string) string {
		if proxyTarget(host) != "" || proxyCodexProcess(host) {
			return host
		}
		return "[主机已隐藏]"
	})
	v, _ = redactedWSMessage(v)
	r := []rune(v)
	if len(r) > limit {
		v = string(r[:limit]) + "…"
	}
	return v
}
func (s *liveWSProxyService) acceptEvent(raw []byte) {
	var event map[string]json.RawMessage
	err := json.Unmarshal(raw, &event)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Received++
	if err != nil {
		s.snapshot.Unrecognized++
		return
	}
	get := func(key string) string { var v string; _ = json.Unmarshal(event[key], &v); return v }
	level := strings.ToLower(get("type"))
	if level == "" {
		level = strings.ToLower(get("level"))
	}
	if level == "" {
		s.snapshot.Unrecognized++
		return
	}
	if level != "warning" && level != "warn" && level != "error" {
		return
	}
	message := get("payload")
	if message == "" {
		message = get("message")
	}
	if message == "" {
		s.snapshot.Unrecognized++
		return
	}
	var metadata map[string]json.RawMessage
	_ = json.Unmarshal(event["metadata"], &metadata)
	meta := func(key string) string { var value string; _ = json.Unmarshal(metadata[key], &value); return value }
	host := proxyTarget(get("host"))
	if host == "" {
		host = proxyTarget(meta("host"))
	}
	if host == "" {
		for _, match := range proxyDomainRE.FindAllString(message, -1) {
			if h := proxyTarget(match); h != "" {
				host = h
				break
			}
		}
	}
	if host == "" {
		return
	}
	process := proxyProcess(get("process"))
	if process == "" {
		process = proxyProcess(get("processPath"))
	}
	if process == "" {
		process = proxyProcess(meta("process"))
	}
	if process == "" {
		process = proxyProcess(meta("processPath"))
	}
	if process == "" {
		if match := proxyProcessRE.FindStringSubmatch(message); len(match) == 2 {
			process = proxyProcess(match[1])
		}
	}
	if process != "" && !proxyCodexProcess(process) {
		return
	}
	association := "OpenAI目标匹配；未关联具体Codex请求（未确认Codex实例）"
	if process != "" {
		association = "OpenAI目标与 " + process + " 进程名匹配；未关联具体Codex请求或实例"
	}
	message = proxySafeText(message, 2000)
	at := time.Now()
	s.snapshot.Matched++
	s.snapshot.Events = append([]liveWSProxyEvent{{proxyHash(at.String() + message), at, level, message, host, process, association}}, s.snapshot.Events...)
	if len(s.snapshot.Events) > 300 {
		s.snapshot.Events = s.snapshot.Events[:300]
	}
	s.snapshot.UpdatedAt = at
}
