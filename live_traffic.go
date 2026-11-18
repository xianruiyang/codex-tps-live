package codextps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Traffic observes the existing controller only. It never installs a proxy,
// enables socket instrumentation, or reads a Codex request/response body.
type liveTrafficBytes struct {
	Upload   int64 `json:"upload"`
	Download int64 `json:"download"`
}

type liveTrafficStatus struct {
	Running                 bool      `json:"running"`
	StartedAt               time.Time `json:"started_at"`
	LastSampleAt            time.Time `json:"last_sample_at"`
	Source                  string    `json:"source"`
	Error                   string    `json:"error"`
	ProcessLookupError      string    `json:"process_lookup_error"`
	ActiveConnections       int       `json:"active_connections"`
	UnidentifiedConnections int       `json:"unidentified_connections"`
	PollSeconds             int       `json:"poll_seconds"`
	TotalSamples            int64     `json:"total_samples"`
	LostConnections         int64     `json:"lost_connections"`
	CounterResets           int64     `json:"counter_resets"`
	InterruptedSamples      int64     `json:"interrupted_samples"`
}

type liveTrafficConnection struct {
	ID               string    `json:"id"`
	Process          string    `json:"process"`
	PID              uint32    `json:"pid"`
	Host             string    `json:"host"`
	Port             string    `json:"port"`
	Network          string    `json:"network"`
	FirstSeen        time.Time `json:"first_seen"`
	LastSeen         time.Time `json:"last_seen"`
	Active           bool      `json:"active"`
	Upload           int64     `json:"upload"`
	Download         int64     `json:"download"`
	BaselineUpload   int64     `json:"baseline_upload"`
	BaselineDownload int64     `json:"baseline_download"`
	Identity         string    `json:"identity"`
	Category         string    `json:"category"`
	Samples          int64     `json:"samples"`
}

type liveTrafficSession struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Model       string `json:"model"`
	ParentID    string `json:"parent_id"`
	Estimated   bool   `json:"estimated"`
	ContextOnly bool   `json:"context_only,omitempty"`
	Upload      int64  `json:"upload"`
	Download    int64  `json:"download"`
	Samples     int64  `json:"samples"`
}

type liveTrafficProcess struct {
	Process     string `json:"process"`
	PID         uint32 `json:"pid"`
	Upload      int64  `json:"upload"`
	Download    int64  `json:"download"`
	Connections int    `json:"connections"`
}

type liveTrafficPoint struct {
	At       time.Time `json:"at"`
	Upload   int64     `json:"upload"`
	Download int64     `json:"download"`
	Seconds  int       `json:"seconds"`
}

type liveTrafficReport struct {
	From                 time.Time               `json:"from"`
	To                   time.Time               `json:"to"`
	Status               liveTrafficStatus       `json:"status"`
	Totals               liveTrafficBytes        `json:"totals"`
	Allocated            liveTrafficBytes        `json:"allocated"`
	Unallocated          liveTrafficBytes        `json:"unallocated"`
	Gap                  liveTrafficBytes        `json:"gap"`
	Points               []liveTrafficPoint      `json:"points"`
	Processes            []liveTrafficProcess    `json:"processes"`
	Sessions             []liveTrafficSession    `json:"sessions"`
	Connections          []liveTrafficConnection `json:"connections"`
	ConnectionCount      int                     `json:"connection_count"`
	ConnectionsTruncated bool                    `json:"connections_truncated"`
}

type liveTrafficCheckpoint struct {
	Connection liveTrafficConnection `json:"connection"`
	Upload     int64                 `json:"upload_counter"`
	Download   int64                 `json:"download_counter"`
	Started    time.Time             `json:"started"`
	At         time.Time             `json:"at"`
}

type liveTrafficWireConnection struct {
	ID       string `json:"id"`
	Upload   int64  `json:"upload"`
	Download int64  `json:"download"`
	Start    string `json:"start"`
	Metadata struct {
		Host            string          `json:"host"`
		SniffHost       string          `json:"sniffHost"`
		Process         string          `json:"process"`
		ProcessPath     string          `json:"processPath"`
		Network         string          `json:"network"`
		SourceIP        string          `json:"sourceIP"`
		SourcePort      json.RawMessage `json:"sourcePort"`
		DestinationPort json.RawMessage `json:"destinationPort"`
	} `json:"metadata"`
}

type liveTrafficCandidate struct {
	Session liveTrafficSession
	Weight  int64
}

// Only inspect already parsed, recent model activity. This does not request
// backfill, produce full session snapshots, or scan raw chat logs every second.
func (m *liveMonitor) trafficCandidates(from, to time.Time) []liveTrafficCandidate {
	m.mu.RLock()
	defer m.mu.RUnlock()
	byID := map[string]liveTrafficCandidate{}
	for _, f := range m.files {
		s := f.session
		if s == nil || !f.initialized || f.offset < f.size || s.ID == "" || s.current == nil {
			continue
		}
		weight := int64(0)
		overlap := func(start, end time.Time) {
			if start.IsZero() || end.IsZero() {
				return
			}
			start, end = maxTime(start, from), minTime(end, to)
			if end.After(start) {
				weight += end.Sub(start).Nanoseconds()
			}
		}
		// The latest turn suffices for a one-second live observation interval.
		for i := len(s.current.Responses) - 1; i >= 0; i-- {
			r := s.current.Responses[i]
			if !r.RecordedAt.IsZero() && r.RecordedAt.Before(from.Add(-2*time.Second)) {
				break
			}
			overlap(r.StartedAt, r.CompletedAt)
		}
		if s.current.Status == "active" && s.start != nil && s.UpdatedAt.After(to.Add(-15*time.Minute)) {
			end := to
			if s.generatedEnd != nil {
				end = *s.generatedEnd
			}
			overlap(maxTime(*s.start, s.current.StartedAt), end)
		}
		if weight == 0 {
			continue
		}
		title := s.Title
		if name := m.names.entries[s.ID].Name; name != "" {
			title = name
		}
		if title == "" {
			title = s.agentName()
		}
		if title == "" {
			title = "未命名对话"
		}
		byID[s.ID] = liveTrafficCandidate{Session: liveTrafficSession{ID: s.ID, Title: shortText(title, 160), Model: s.model, ParentID: s.ParentID}, Weight: weight}
	}
	out := make([]liveTrafficCandidate, 0, len(byID))
	for _, c := range byID {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session.ID < out[j].Session.ID })
	return out
}

// Supplement the display tree from cached metadata only. Parent rows have no
// usage of their own added here, so neither history nor byte totals change.
func (m *liveMonitor) completeTrafficTree(report *liveTrafficReport) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	files := m.sessions()
	indices := make(map[string]int, len(report.Sessions))
	for i, session := range report.Sessions {
		indices[session.ID] = i
	}
	for i := 0; i < len(report.Sessions); i++ {
		session := &report.Sessions[i]
		if f := files[session.ID]; f != nil {
			s := f.session
			if session.ContextOnly || session.Title == "" || session.Title == "未命名对话" {
				session.Title = s.Title
				if session.Title == "" {
					session.Title = s.agentName()
				}
			}
			if session.ContextOnly {
				session.Model = s.model
			}
			if session.ParentID == "" {
				session.ParentID = s.ParentID
			}
		}
		if name := m.names.entries[session.ID].Name; name != "" {
			session.Title = name
		}
		if session.Title == "" {
			session.Title = "未命名对话"
		}
		parent := session.ParentID
		if parent == "" || parent == session.ID {
			continue
		}
		if _, exists := indices[parent]; exists {
			continue
		}
		indices[parent] = len(report.Sessions)
		report.Sessions = append(report.Sessions, liveTrafficSession{ID: parent, Title: "未命名对话", ContextOnly: true, Estimated: true})
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

type liveTrafficService struct {
	mu          sync.Mutex
	status      liveTrafficStatus
	store       *liveTrafficStore
	checkpoints map[string]liveTrafficCheckpoint
	home        string
	monitor     *liveMonitor
	cancel      context.CancelFunc
	done        chan struct{}
}

func openLiveTraffic(ctx context.Context, monitor *liveMonitor, clashHome, dataDir string) *liveTrafficService {
	s := &liveTrafficService{home: clashHome, monitor: monitor, done: make(chan struct{}), checkpoints: map[string]liveTrafficCheckpoint{}}
	s.status = liveTrafficStatus{PollSeconds: 1}
	var err error
	s.store, s.checkpoints, s.status, err = openLiveTrafficStore(monitor.home, dataDir)
	if err != nil {
		s.status.Error = err.Error()
		close(s.done)
		return s
	}
	child, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	go func() { defer close(s.done); s.run(child) }()
	return s
}

func (s *liveTrafficService) close() {
	if s.cancel != nil {
		s.cancel()
	}
	<-s.done
	if s.store != nil {
		s.store.close()
	}
}

func (s *liveTrafficService) register(mux *http.ServeMux) {
	mux.HandleFunc("/traffic", func(w http.ResponseWriter, r *http.Request) {
		page, _ := liveAssets.ReadFile("web/traffic.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
	mux.HandleFunc("/api/traffic", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		from, to, err := liveTrendRange(r)
		if err != nil || to.Sub(from) > 31*24*time.Hour {
			http.Error(w, "请选择不超过 31 天的有效时间范围", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		status := s.status
		s.mu.Unlock()
		end := to.Truncate(time.Minute)
		if end.Before(to) {
			end = end.Add(time.Minute)
		}
		report := liveTrafficReport{From: from.Truncate(time.Minute), To: end, Status: status, Points: []liveTrafficPoint{}, Processes: []liveTrafficProcess{}, Sessions: []liveTrafficSession{}, Connections: []liveTrafficConnection{}}
		if s.store != nil {
			if err := s.store.report(r.Context(), &report); err != nil {
				report.Status.Error = "流量历史读取失败"
			}
		}
		s.monitor.completeTrafficTree(&report)
		writeLiveJSON(w, report)
	})
}

func (s *liveTrafficService) run(ctx context.Context) {
	var controller liveWSProxyController
	var client *http.Client
	var transport *http.Transport
	defer func() {
		if transport != nil {
			transport.CloseIdleConnections()
		}
	}()
	for ctx.Err() == nil {
		var err error
		if client == nil {
			controller, err = discoverLiveWSProxyController(s.home)
			if err == nil {
				transport = &http.Transport{DialContext: controller.dial, Proxy: nil, MaxIdleConns: 1, MaxIdleConnsPerHost: 1, IdleConnTimeout: 30 * time.Second}
				client = &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			}
		}
		if err == nil {
			err = s.poll(ctx, client, controller)
		}
		if ctx.Err() != nil {
			return
		}
		delay := time.Second
		if err != nil {
			s.mu.Lock()
			s.status.Running = false
			s.status.Error = proxyControllerError(err)
			if controller.source == "" {
				s.status.Error = err.Error()
			}
			if strings.HasPrefix(err.Error(), "流量记录") {
				s.status.Error = "流量记录写入失败；保留上次检查点，正在重试"
			}
			s.mu.Unlock()
			if transport != nil {
				transport.CloseIdleConnections()
			}
			client = nil
			delay = 5 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *liveTrafficService) poll(ctx context.Context, client *http.Client, controller liveWSProxyController) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+controller.address+"/connections", nil)
	if controller.secret != "" {
		req.Header.Set("Authorization", "Bearer "+controller.secret)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return proxyHTTPError(resp.StatusCode)
	}
	var body struct {
		Connections []liveTrafficWireConnection `json:"connections"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body) != nil {
		return errors.New("connections 无法解析")
	}
	at := time.Now().UTC()
	s.mu.Lock()
	status := s.status
	s.mu.Unlock()
	previous := status.LastSampleAt
	gap := !previous.IsZero() && (!status.Running || at.Sub(previous) > 5*time.Second)
	var candidates []liveTrafficCandidate
	if !previous.IsZero() && !gap {
		candidates = s.monitor.trafficCandidates(previous, at)
	}
	owners, lookupErr := liveTrafficOwners(body.Connections)
	status.ProcessLookupError = lookupErr
	status.ActiveConnections, status.UnidentifiedConnections = 0, 0
	status.Running, status.Error, status.Source = true, "", controller.source
	status.LastSampleAt = at
	status.TotalSamples++
	if status.StartedAt.IsZero() {
		status.StartedAt = at
	}
	if gap {
		status.InterruptedSamples++
	}
	changed := map[string]liveTrafficCheckpoint{}
	seen := map[string]bool{}
	var deltas []liveTrafficDelta
	for _, raw := range body.Connections {
		if raw.ID == "" || raw.Upload < 0 || raw.Download < 0 {
			continue
		}
		id := proxyHash(raw.ID + "\x00" + raw.Start)
		old, known := s.checkpoints[id]
		process := proxyProcess(raw.Metadata.Process)
		if process == "" {
			process = proxyProcess(raw.Metadata.ProcessPath)
		}
		owner := owners[liveTrafficSocketKey(raw.Metadata.SourceIP, liveTrafficPort(raw.Metadata.SourcePort))]
		identity := "代理进程字段"
		if !proxyCodexProcess(process) && proxyCodexProcess(owner.Process) {
			process, identity = owner.Process, "Windows 连接与进程匹配"
		}
		if !proxyCodexProcess(process) {
			if known && old.Connection.Active {
				process, identity = old.Connection.Process, old.Connection.Identity
			} else {
				if process == "" && proxyTarget(raw.Metadata.Host) != "" {
					status.UnidentifiedConnections++
				}
				continue
			}
		}
		if !known {
			old, known, err = s.store.checkpoint(ctx, id)
			if err != nil {
				return fmt.Errorf("流量记录检查点读取失败: %w", err)
			}
		}
		status.ActiveConnections++
		seen[id] = true
		next := old
		started, _ := time.Parse(time.RFC3339Nano, raw.Start)
		if !known {
			next = liveTrafficCheckpoint{Connection: liveTrafficConnection{ID: id, Process: process, Identity: identity, FirstSeen: at}, Started: started}
			// Existing connections are a baseline, not retroactive traffic. Only a
			// connection known to start after the preceding successful sample can
			// safely contribute its complete first observed counters.
			if previous.IsZero() || started.IsZero() || started.Before(previous) || gap {
				next.Upload, next.Download = raw.Upload, raw.Download
				next.Connection.BaselineUpload, next.Connection.BaselineDownload = raw.Upload, raw.Download
			}
		}
		upload, download := raw.Upload-next.Upload, raw.Download-next.Download
		if upload < 0 || download < 0 {
			// A reset is a fresh unknown baseline, never a negative usage delta.
			upload, download = 0, 0
			next.Connection.BaselineUpload += raw.Upload
			next.Connection.BaselineDownload += raw.Download
			status.CounterResets++
		}
		next.Upload, next.Download, next.At = raw.Upload, raw.Download, at
		next.Connection.Active, next.Connection.LastSeen = true, at
		next.Connection.Process, next.Connection.Identity = process, identity
		if owner.PID != 0 && strings.EqualFold(owner.Process, process) {
			next.Connection.PID = owner.PID
		}
		host := raw.Metadata.Host
		if host == "" {
			host = raw.Metadata.SniffHost
		}
		next.Connection.Host = shortText(strings.ToLower(host), 253)
		next.Connection.Port = liveTrafficPort(raw.Metadata.DestinationPort)
		next.Connection.Network = shortText(raw.Metadata.Network, 16)
		next.Connection.Category = "other"
		if liveTrafficModelCandidate(process, host, raw.Metadata.Network) {
			next.Connection.Category = "model_candidate"
		}
		next.Connection.Upload += upload
		next.Connection.Download += download
		next.Connection.Samples++
		changed[id] = next
		if upload == 0 && download == 0 {
			continue
		}
		connectionGap := gap || (known && (!old.Connection.Active || at.Sub(old.At) > 5*time.Second))
		allocation := candidates
		if connectionGap || next.Connection.Category != "model_candidate" {
			allocation = nil
		}
		deltas = append(deltas, splitLiveTraffic(at, id, upload, download, connectionGap, allocation)...)
	}
	for id, old := range s.checkpoints {
		if old.Connection.Active && !seen[id] {
			old.Connection.Active = false
			changed[id] = old
			status.LostConnections++
		}
	}
	if err := s.store.save(ctx, changed, deltas, candidates, status); err != nil {
		return fmt.Errorf("流量记录写入失败: %w", err)
	}
	for id, next := range changed {
		if next.Connection.Active {
			s.checkpoints[id] = next
		} else {
			delete(s.checkpoints, id)
		}
	}
	s.mu.Lock()
	s.status = status
	s.mu.Unlock()
	return nil
}

// A broad *.chatgpt.com match also captures desktop background traffic. Only
// the Codex engine's standard model hosts are eligible for conversation
// estimates. The endpoint path is unavailable, so even these remain estimates.
func liveTrafficModelCandidate(process, host, network string) bool {
	if !strings.EqualFold(process, "codex.exe") || !strings.EqualFold(network, "tcp") {
		return false
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	return host == "chatgpt.com" || host == "api.openai.com"
}

func liveTrafficPort(raw json.RawMessage) string {
	n, err := strconv.ParseUint(strings.Trim(string(raw), "\""), 10, 16)
	if err != nil || n == 0 {
		return ""
	}
	return strconv.FormatUint(n, 10)
}

type liveTrafficDelta struct {
	At                  int64
	Connection, Session string
	Upload, Download    int64
	Gap                 bool
}

// All allocations are estimates. Integer remainders are retained so session
// estimates plus unallocated bytes always equal the observed connection total.
func splitLiveTraffic(at time.Time, id string, upload, download int64, gap bool, candidates []liveTrafficCandidate) []liveTrafficDelta {
	base := liveTrafficDelta{At: at.Truncate(time.Minute).Unix(), Connection: id, Upload: upload, Download: download, Gap: gap}
	var weight int64
	for _, c := range candidates {
		if c.Weight > 0 {
			weight += c.Weight
		}
	}
	if weight == 0 {
		return []liveTrafficDelta{base}
	}
	out := make([]liveTrafficDelta, 0, len(candidates))
	leftUp, leftDown, leftWeight := upload, download, weight
	for _, c := range candidates {
		if c.Weight <= 0 {
			continue
		}
		up := int64(float64(leftUp) * (float64(c.Weight) / float64(leftWeight)))
		down := int64(float64(leftDown) * (float64(c.Weight) / float64(leftWeight)))
		if c.Weight == leftWeight {
			up, down = leftUp, leftDown
		}
		out = append(out, liveTrafficDelta{At: base.At, Connection: id, Session: c.Session.ID, Upload: up, Download: down, Gap: gap})
		leftUp, leftDown, leftWeight = leftUp-up, leftDown-down, leftWeight-c.Weight
	}
	return out
}
