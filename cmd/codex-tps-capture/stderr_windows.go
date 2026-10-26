//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"
)

type stderrChunk struct {
	sequence uint64
	data     []byte
}

type stderrTap struct {
	queue   chan stderrChunk
	done    chan struct{}
	seq     uint64
	dropped atomic.Uint64
	records atomic.Uint64
	dir     string
	name    string
}

type capturedTiming struct {
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

func newStderrTap(cfg captureConfig) *stderrTap {
	dir, _ := captureDir()
	tap := &stderrTap{queue: make(chan stderrChunk, 32), done: make(chan struct{}), dir: dir,
		name: fmt.Sprintf("ws-timing-%s-%d.jsonl", time.Now().UTC().Format("20060102T150405"), os.Getpid())}
	go tap.consume()
	return tap
}

func (t *stderrTap) Write(p []byte) (int, error) {
	// Preserve stderr for the desktop first. Capture never waits for its disk
	// worker: at most 512 KiB is queued, and missing chunks invalidate a line.
	_, _ = os.Stderr.Write(p)
	n := len(p)
	for rest := p[:n]; len(rest) > 0; {
		length := min(len(rest), 16*1024)
		t.seq++
		if len(t.queue) == cap(t.queue) {
			t.dropped.Add(1)
			rest = rest[length:]
			continue
		}
		select {
		case t.queue <- stderrChunk{sequence: t.seq, data: bytes.Clone(rest[:length])}:
		default:
			t.dropped.Add(1)
		}
		rest = rest[length:]
	}
	return n, nil
}

func (t *stderrTap) started(pid int) {
	// A tiny local marker distinguishes "not restarted" from "connected but no
	// timing received". Failures affect collection only, never the original CLI.
	data, _ := json.Marshal(map[string]any{"wrapper_pid": os.Getpid(), "codex_pid": pid, "started_at": time.Now().UTC(), "log": t.name})
	_ = atomicCaptureWrite(filepath.Join(t.dir, "active.json"), append(data, '\n'))
}

func (t *stderrTap) finish(timeout time.Duration) {
	close(t.queue)
	select {
	case <-t.done:
	case <-time.After(timeout):
	}
}

func (t *stderrTap) consume() {
	defer close(t.done)
	var line []byte
	var sequence uint64
	discard := false
	for chunk := range t.queue {
		if chunk.sequence != sequence+1 {
			line, discard = nil, true
		}
		sequence = chunk.sequence
		for _, b := range chunk.data {
			if b == '\n' {
				if !discard {
					if record, ok := parseCapturedTiming(line); ok {
						if t.appendRecord(record) == nil {
							t.records.Add(1)
						}
					}
				}
				line, discard = line[:0], false
			} else if !discard {
				if len(line) >= 64*1024 {
					line, discard = nil, true
					t.dropped.Add(1)
				} else {
					line = append(line, b)
				}
			}
		}
	}
	data, _ := json.Marshal(map[string]any{"wrapper_pid": os.Getpid(), "records": t.records.Load(), "dropped_chunks": t.dropped.Load(), "ended_at": time.Now().UTC()})
	_ = atomicCaptureWrite(filepath.Join(t.dir, "last-run.json"), append(data, '\n'))
}

func parseCapturedTiming(line []byte) (capturedTiming, bool) {
	var record capturedTiming
	if !bytes.Contains(line, []byte(captureTarget)) {
		return record, false
	}
	var event struct {
		Timestamp string                     `json:"timestamp"`
		Target    string                     `json:"target"`
		Fields    map[string]json.RawMessage `json:"fields"`
	}
	if json.Unmarshal(line, &event) != nil || event.Target != captureTarget {
		return record, false
	}
	if event.Fields == nil {
		_ = json.Unmarshal(line, &event.Fields)
	}
	var payload map[string]json.RawMessage
	raw := event.Fields["payload"]
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return record, false
		}
		raw = []byte(text)
	}
	if json.Unmarshal(raw, &payload) != nil {
		return record, false
	}
	var metrics map[string]json.RawMessage
	if json.Unmarshal(payload["timing_metrics"], &metrics) != nil {
		return record, false
	}
	record.Metrics = map[string]float64{}
	for _, key := range []string{"responses_duration_excl_engine_and_client_tool_time_ms", "engine_service_total_ms", "engine_iapi_ttft_total_ms", "engine_service_ttft_total_ms", "engine_iapi_tbt_across_engine_calls_ms", "engine_service_tbt_across_engine_calls_ms"} {
		var value float64
		if json.Unmarshal(metrics[key], &value) == nil && value >= 0 {
			record.Metrics[key] = value
		}
	}
	if len(record.Metrics) == 0 {
		return record, false
	}
	record.At, _ = time.Parse(time.RFC3339Nano, event.Timestamp)
	if record.At.IsZero() {
		record.At = time.Now().UTC()
	}
	value := func(m map[string]json.RawMessage, key string, limit int) string {
		var text string
		_ = json.Unmarshal(m[key], &text)
		if len(text) > limit {
			return ""
		}
		return text
	}
	start := value(event.Fields, "request_start_ms", 24)
	if start == "" {
		start = string(event.Fields["request_start_ms"])
	}
	record.RequestStartMS, _ = strconv.ParseInt(start, 10, 64)
	record.ThreadID = value(event.Fields, "thread_id", 64)
	record.TurnID = value(event.Fields, "turn_id", 64)
	record.Model = value(event.Fields, "model", 128)
	record.PreviousResponseID = value(event.Fields, "previous_response_id", 256)
	record.ResponseID = value(payload, "response_id", 256)
	_ = json.Unmarshal(event.Fields["warmup"], &record.Warmup)
	_ = json.Unmarshal(event.Fields["connection_reused"], &record.ConnectionReused)
	return record, true
}

func (t *stderrTap) appendRecord(record capturedTiming) error {
	path := filepath.Join(t.dir, t.name)
	if info, err := os.Stat(path); err == nil && info.Size() >= 4*1024*1024 {
		// Only this process's own file is rotated. There is no cross-process
		// shared writer and no recursive deletion of any directory.
		_ = os.Remove(path + ".previous")
		if err = os.Rename(path, path+".previous"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(record)
}

var _ io.Writer = (*stderrTap)(nil)

// Deliberately no raw payload, prompt text, tool output, auth header, or URI is
// persisted by this collector. Unknown fields are discarded rather than copied.
