package codextps

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The optional capture executable writes only this metadata whitelist, never
// raw stderr, request bodies, user text, or credentials.
type liveCapturedTiming struct {
	At                 time.Time          `json:"at"`
	RequestStartMS     int64              `json:"request_start_ms"`
	ThreadID           string             `json:"thread_id"`
	TurnID             string             `json:"turn_id"`
	Model              string             `json:"model"`
	ResponseID         string             `json:"response_id,omitempty"`
	PreviousResponseID string             `json:"previous_response_id,omitempty"`
	Warmup             bool               `json:"warmup"`
	ConnectionReused   bool               `json:"connection_reused"`
	Metrics            map[string]float64 `json:"timing_metrics"`
}

type liveCaptureStatus struct {
	Prepared bool   `json:"prepared"`
	Running  bool   `json:"running"`
	Records  int    `json:"records"`
	Pending  bool   `json:"pending"`
	Message  string `json:"message"`
}

type liveCaptureFile struct {
	forward  int64
	backfill int64
	modified time.Time
}

type liveCaptureReader struct {
	dir      string
	lastRead time.Time
	files    map[string]*liveCaptureFile
	turn     int
}

func (r *liveCaptureReader) read(now time.Time) ([]liveCapturedTiming, liveCaptureStatus, error) {
	var status liveCaptureStatus
	if r.dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, status, err
		}
		r.dir = filepath.Join(base, "CodexTPSLive", "timing")
	}
	var config struct {
		Enabled bool `json:"enabled"`
	}
	if data, err := os.ReadFile(filepath.Join(r.dir, "capture.json")); err == nil {
		_ = json.Unmarshal(data, &config)
	}
	status.Prepared = config.Enabled
	var active struct {
		PID int `json:"wrapper_pid"`
	}
	if data, err := os.ReadFile(filepath.Join(r.dir, "active.json")); err == nil {
		_ = json.Unmarshal(data, &active)
		status.Running = status.Prepared && captureProcessAlive(active.PID)
	}
	entries, err := os.ReadDir(r.dir)
	if os.IsNotExist(err) {
		return nil, status, nil
	}
	if err != nil {
		return nil, status, err
	}
	type candidate struct {
		path string
		info os.FileInfo
	}
	var files []candidate
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasPrefix(name, "ws-timing-") && (strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".jsonl.previous")) {
			if info, infoErr := entry.Info(); infoErr == nil {
				files = append(files, candidate{filepath.Join(r.dir, name), info})
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].info.ModTime().After(files[j].info.ModTime()) })
	if len(files) > 64 {
		files = files[:64]
	}
	if r.files == nil {
		r.files = map[string]*liveCaptureFile{}
	}
	if len(files) == 0 {
		return nil, status, nil
	}
	// Read at most one bounded region per second. The newest file gets the
	// first pass; older regions/files are filled in without starving live appends.
	index := r.turn % len(files)
	r.turn++
	if r.turn%2 == 1 {
		for i, f := range files {
			if old := r.files[f.path]; old == nil || old.forward < f.info.Size() {
				index = i
				break
			}
		}
	}
	f := files[index]
	state := r.files[f.path]
	const chunkSize int64 = 256 * 1024
	initial := state == nil || f.info.Size() < state.forward || (f.info.Size() == state.forward && !state.modified.Equal(f.info.ModTime()))
	if initial {
		start := max(int64(0), f.info.Size()-chunkSize)
		state = &liveCaptureFile{forward: start, backfill: start}
		r.files[f.path] = state
	}
	state.modified = f.info.ModTime()
	start, end := state.forward, f.info.Size()
	backfill := !initial && state.backfill > 0 && (start >= end || r.turn%2 == 0)
	if backfill {
		start = max(int64(0), state.backfill-chunkSize)
		end = min(f.info.Size(), state.backfill+64*1024)
	} else {
		end = min(end, start+chunkSize)
	}
	stream, err := os.Open(f.path)
	if err != nil {
		return nil, status, err
	}
	data, err := io.ReadAll(io.NewSectionReader(stream, start, end-start))
	stream.Close()
	if err != nil {
		return nil, status, err
	}
	consumed := int64(0)
	last := bytes.LastIndexByte(data, '\n')
	if last >= 0 {
		consumed = int64(last + 1)
		data = data[:last]
	} else {
		data = nil
	}
	if start > 0 && (initial || backfill) {
		if first := bytes.IndexByte(data, '\n'); first >= 0 {
			data = data[first+1:]
		} else {
			data = nil
		}
	}
	var result []liveCapturedTiming
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) > 64*1024 {
			continue
		}
		var record liveCapturedTiming
		if json.Unmarshal(line, &record) == nil && record.RequestStartMS > 0 && record.ThreadID != "" && record.TurnID != "" && len(record.Metrics) > 0 {
			result = append(result, record)
		}
	}
	if backfill {
		state.backfill = start
	} else {
		state.forward = start + consumed
	}
	for _, f := range files {
		state := r.files[f.path]
		if state == nil || state.forward < f.info.Size() || state.backfill > 0 {
			status.Pending = true
			break
		}
	}
	return result, status, nil
}

func (m *liveMonitor) pollCapture(now time.Time) {
	if m.captureReader == nil {
		m.captureReader = &liveCaptureReader{}
	}
	if now.Sub(m.captureReader.lastRead) < time.Second {
		return
	}
	m.captureReader.lastRead = now
	records, status, err := m.captureReader.read(now)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.captureRecords == nil {
		m.captureRecords = map[string]liveCapturedTiming{}
	}
	for _, record := range records {
		key := record.ThreadID + "|" + record.TurnID + "|" + time.UnixMilli(record.RequestStartMS).Format(time.RFC3339Nano) + "|" + record.ResponseID + "|" + record.PreviousResponseID
		m.captureRecords[key] = record
	}
	if len(records) > 0 {
		ordered := make([]liveCapturedTiming, 0, len(m.captureRecords))
		for _, record := range m.captureRecords {
			ordered = append(ordered, record)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].RequestStartMS > ordered[j].RequestStartMS })
		if len(ordered) > 65536 {
			ordered = ordered[:65536]
			m.captureRecords = map[string]liveCapturedTiming{}
			for _, record := range ordered {
				key := record.ThreadID + "|" + record.TurnID + "|" + time.UnixMilli(record.RequestStartMS).Format(time.RFC3339Nano) + "|" + record.ResponseID + "|" + record.PreviousResponseID
				m.captureRecords[key] = record
			}
		}
		m.captureThreads = map[string][]liveCapturedTiming{}
		for _, record := range ordered {
			m.captureThreads[record.ThreadID] = append(m.captureThreads[record.ThreadID], record)
		}
	}
	status.Records = len(m.captureRecords)
	switch {
	case err != nil:
		status.Message = "WebSocket 计时采集读取暂不可用：" + err.Error()
	case status.Running && status.Records == 0:
		status.Message = "WebSocket 计时采集器已接入，尚未收到可关联的服务端 timing。"
	case status.Running:
		status.Message = "WebSocket 计时采集器已接入；服务端 TTFT 使用独立来源标注，不等同于客户端首内容等待。"
	case status.Prepared:
		status.Message = "WebSocket 计时已准备：工作结束后退出 Codex，再用“启动Codex计时采集.cmd”启动生效。"
	default:
		status.Message = "WebSocket 定向计时未启用；可选启动器仅旁读计时日志，不代理模型请求。"
	}
	m.captureStatus = status
}

func (m *liveMonitor) capturedFirstToken(s *liveSession, turn *liveTurn, r *liveResponse) *liveFirstToken {
	var candidates []liveCapturedTiming
	for _, record := range m.captureThreads[s.ID] {
		if record.Warmup || record.TurnID != turn.ID || record.RequestStartMS < r.StartedAt.UnixMilli() || record.RequestStartMS > r.CompletedAt.UnixMilli() {
			continue
		}
		if record.Model != "" && r.Model != "" && record.Model != r.Model {
			continue
		}
		candidates = append(candidates, record)
	}
	if len(candidates) != 1 {
		return nil // do not force retry/parallel timing into one usage checkpoint
	}
	record := candidates[0]
	key := "engine_iapi_ttft_total_ms"
	wait := record.Metrics[key] / 1000
	if wait <= 0 {
		key = "engine_service_ttft_total_ms"
		wait = record.Metrics[key] / 1000
	}
	if wait <= 0 || wait >= r.Seconds {
		return nil
	}
	return &liveFirstToken{Seconds: wait, Source: "websocket_server_timing", Estimated: true, Note: "Codex 原生 WebSocket timing 的 " + key + "；按同对话、同轮次及响应边界内唯一 request_start_ms 关联。服务端引擎计时不包含客户端准备与网络等待，total 字段可能累计请求内部多个 engine call；这是实际 TPS 的估算依据，不是客户端逐 token 实测。"}
}
