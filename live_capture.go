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
	EventID            string             `json:"event_id"`
	Shape              string             `json:"shape"`
	Scope              string             `json:"scope,omitempty"`
	CriticalScope      string             `json:"critical_scope,omitempty"`
	Coverage           string             `json:"coverage,omitempty"`
	Boundary           string             `json:"boundary_type,omitempty"`
	CriticalPath       map[string]float64 `json:"critical_path,omitempty"`
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

type liveCaptureDiagnostics struct {
	Events            uint64 `json:"events"`
	Written           uint64 `json:"written"`
	Malformed         uint64 `json:"malformed"`
	UnknownSchema     uint64 `json:"unknown_schema"`
	IncompleteContext uint64 `json:"incomplete_context"`
	WriteErrors       uint64 `json:"write_errors"`
	DroppedChunks     uint64 `json:"dropped_chunks"`
	OversizeLines     uint64 `json:"oversize_lines"`
	FlushIncomplete   bool   `json:"flush_incomplete"`
	LastError         string `json:"last_error,omitempty"`
	Warning           string `json:"warning,omitempty"`
}

type liveCaptureRunState struct {
	WrapperCreated int64 `json:"wrapper_created"`
	BackendCreated int64 `json:"backend_created"`
	liveCaptureDiagnostics
	PID       int       `json:"wrapper_pid"`
	CodexPID  int       `json:"codex_pid"`
	Records   uint64    `json:"records"`
	UpdatedAt time.Time `json:"updated_at"`
	Phase     string    `json:"phase"`
}

type liveCaptureStatus struct {
	liveCaptureDiagnostics
	Prepared       bool   `json:"prepared"`
	Running        bool   `json:"running"`
	Records        int    `json:"records"`
	Pending        bool   `json:"pending"`
	DiagnosticOnly bool   `json:"diagnostic_only"`
	HeartbeatStale bool   `json:"heartbeat_stale"`
	HasErrors      bool   `json:"has_errors"`
	Message        string `json:"message"`
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
		if err = json.Unmarshal(data, &config); err != nil {
			return nil, status, err
		}
	} else if !os.IsNotExist(err) {
		return nil, status, err
	}
	status.Prepared, status.DiagnosticOnly = config.Enabled, true
	var active liveCaptureRunState
	if data, err := os.ReadFile(filepath.Join(r.dir, "active.json")); err == nil {
		if err = json.Unmarshal(data, &active); err != nil {
			return nil, status, err
		}
		status.Running = captureProcessAlive(active.PID, active.WrapperCreated) && captureBackendAlive(active.CodexPID, active.BackendCreated)
		if !status.Running {
			var last liveCaptureRunState
			if data, lastErr := os.ReadFile(filepath.Join(r.dir, "last-run.json")); lastErr == nil && json.Unmarshal(data, &last) == nil && last.PID == active.PID && last.WrapperCreated == active.WrapperCreated && !last.UpdatedAt.Before(active.UpdatedAt) {
				active = last
			}
		}
		status.liveCaptureDiagnostics = active.liveCaptureDiagnostics
		status.Written = active.Records
		status.HeartbeatStale = status.Running && (active.UpdatedAt.IsZero() || now.Sub(active.UpdatedAt) > 5*time.Second)
	} else if !os.IsNotExist(err) {
		return nil, status, err
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
		if json.Unmarshal(line, &record) == nil && (record.EventID != "" || record.Shape != "" || len(record.Metrics) > 0) {
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
		key := capturedRecordKey(record)
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
				key := capturedRecordKey(record)
				m.captureRecords[key] = record
			}
		}
	}

	status.Records = len(m.captureRecords)
	status.HasErrors = err != nil || status.Malformed > 0 || status.UnknownSchema > 0 || status.WriteErrors > 0 || status.DroppedChunks > 0 || status.OversizeLines > 0 || status.FlushIncomplete || status.HeartbeatStale || status.Warning != ""
	switch {
	case err != nil:
		status.Message = "服务端计时诊断读取失败：" + err.Error()
	case status.Running && status.HeartbeatStale:
		status.Message = "采集进程仍存活，但状态心跳未更新；不能确认日志采集健康。"
	case status.Running && status.Events == 0:
		status.Message = "采集器与原版后端进程存活，尚未观察到服务端计时事件；客户端逐次 TTFT 未验证支持。"
	case status.Running:
		status.Message = "已观察到服务端计时诊断事件；这些引擎或累计指标不等同于客户端逐次 TTFT，未用于扣除等待或计算实际 TPS。"
	case status.Prepared:
		status.Message = "已保存可选诊断启动信息，当前未采集；当前 Codex 可继续工作，无需为此退出。客户端逐次 TTFT 尚未验证支持。"
	default:
		status.Message = "可选服务端计时诊断未运行；默认只读监控照常工作。"
	}
	m.captureStatus = status
}

func capturedRecordKey(record liveCapturedTiming) string {
	if record.EventID != "" {
		return record.EventID
	}
	return record.ThreadID + "|" + record.TurnID + "|" + time.UnixMilli(record.RequestStartMS).Format(time.RFC3339Nano) + "|" + record.At.Format(time.RFC3339Nano) + "|" + record.ResponseID + "|" + record.PreviousResponseID
}
