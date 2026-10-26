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

type captureRunState struct {
	WrapperCreated    int64     `json:"wrapper_created"`
	BackendCreated    int64     `json:"backend_created"`
	PID               int       `json:"wrapper_pid"`
	CodexPID          int       `json:"codex_pid"`
	LaunchID          string    `json:"launch_id"`
	StartedAt         time.Time `json:"started_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	Phase             string    `json:"phase"`
	Events            uint64    `json:"events"`
	Records           uint64    `json:"records"`
	Malformed         uint64    `json:"malformed"`
	UnknownSchema     uint64    `json:"unknown_schema"`
	IncompleteContext uint64    `json:"incomplete_context"`
	WriteErrors       uint64    `json:"write_errors"`
	DroppedChunks     uint64    `json:"dropped_chunks"`
	OversizeLines     uint64    `json:"oversize_lines"`
	FlushIncomplete   bool      `json:"flush_incomplete"`
	LastError         string    `json:"last_error,omitempty"`
	Warning           string    `json:"warning,omitempty"`
}

type stderrTap struct {
	identities                                                                      [2]atomic.Int64
	queue                                                                           chan stderrChunk
	done                                                                            chan struct{}
	seq                                                                             uint64
	dropped, oversize, records, events, malformed, unknown, incomplete, writeErrors atomic.Uint64
	child, startedAt                                                                atomic.Int64
	flushIncomplete                                                                 atomic.Bool
	lastError, warning                                                              atomic.Value
	dir, name, launchID                                                             string
}

func newStderrTap(cfg captureConfig) *stderrTap {
	dir, _ := captureDir()
	tap := &stderrTap{queue: make(chan stderrChunk, 32), done: make(chan struct{}), dir: dir,
		name:     fmt.Sprintf("ws-timing-%s-%d.jsonl", time.Now().UTC().Format("20060102T150405"), os.Getpid()),
		launchID: os.Getenv("CODEX_TPS_CAPTURE_LAUNCH_ID")}
	go tap.consume()
	return tap
}

func (t *stderrTap) Write(p []byte) (int, error) {
	// Preserve the desktop's stderr first; only the extra copy is nonblocking.
	_, _ = os.Stderr.Write(p)
	for rest := p; len(rest) > 0; {
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
	return len(p), nil
}

func (t *stderrTap) started(pid int) {
	t.identities[0].Store(capturePIDIdentity(os.Getpid(), "codex-tps-capture.exe"))
	t.identities[1].Store(capturePIDIdentity(pid, "codex.exe"))
	t.startedAt.Store(time.Now().UTC().UnixNano())
	t.child.Store(int64(pid))
	t.writeState("backend_started", false)
}

func (t *stderrTap) finish(timeout time.Duration) {
	close(t.queue)
	select {
	case <-t.done:
	case <-time.After(timeout):
		t.flushIncomplete.Store(true)
		t.writeState("flush_incomplete", true)
	}
}

func (t *stderrTap) writeState(phase string, final bool) {
	if t.child.Load() == 0 {
		return
	}
	state := captureRunState{WrapperCreated: t.identities[0].Load(), BackendCreated: t.identities[1].Load(), PID: os.Getpid(), CodexPID: int(t.child.Load()), LaunchID: t.launchID,
		StartedAt: time.Unix(0, t.startedAt.Load()).UTC(), UpdatedAt: time.Now().UTC(), Phase: phase,
		Events: t.events.Load(), Records: t.records.Load(), Malformed: t.malformed.Load(),
		UnknownSchema: t.unknown.Load(), IncompleteContext: t.incomplete.Load(), WriteErrors: t.writeErrors.Load(),
		DroppedChunks: t.dropped.Load(), OversizeLines: t.oversize.Load(), FlushIncomplete: t.flushIncomplete.Load()}
	if v := t.lastError.Load(); v != nil {
		state.LastError = v.(string)
	}
	if v := t.warning.Load(); v != nil {
		state.Warning = v.(string)
	}
	name := "active.json"
	if final {
		name = "last-run.json"
	}
	data, _ := json.Marshal(state)
	if atomicCaptureWrite(filepath.Join(t.dir, name), append(data, '\n')) != nil {
		t.writeErrors.Add(1)
		t.lastError.Store("status_write_failed")
	}
}

func (t *stderrTap) acceptLine(line []byte) {
	record, kind := parseCapturedTiming(line)
	if kind == "" {
		return
	}
	event := t.events.Add(1)
	if kind == "malformed_event" || kind == "malformed_payload" {
		t.malformed.Add(1)
		t.lastError.Store(kind)
		return
	}
	if kind == "schema_unrecognized" {
		t.unknown.Add(1)
	}
	if record.RequestStartMS <= 0 || record.ThreadID == "" || record.TurnID == "" {
		t.incomplete.Add(1)
	}
	record.EventID = t.name + ":" + strconv.FormatUint(event, 10)
	if t.appendRecord(record) != nil {
		t.writeErrors.Add(1)
		t.lastError.Store("record_write_failed")
		return
	}
	t.records.Add(1)
}

func (t *stderrTap) consume() {
	defer close(t.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var line []byte
	var sequence uint64
	discard := false
	for {
		select {
		case <-ticker.C:
			t.writeState("backend_started", false)
		case chunk, ok := <-t.queue:
			if !ok {
				if len(line) > 0 || discard {
					t.dropped.Add(1)
				}
				t.writeState("stopped", true)
				return
			}
			if chunk.sequence != sequence+1 {
				line, discard = nil, true
			}
			sequence = chunk.sequence
			for _, b := range chunk.data {
				if b == '\n' {
					if !discard {
						t.acceptLine(line)
					}
					line, discard = line[:0], false
				} else if !discard {
					if len(line) >= 64*1024 {
						line, discard = nil, true
						t.oversize.Add(1)
					} else {
						line = append(line, b)
					}
				}
			}
		}
	}
}

func (t *stderrTap) appendRecord(record capturedTiming) error {
	path := filepath.Join(t.dir, t.name)
	if info, err := os.Stat(path); err == nil && info.Size() >= 4*1024*1024 {
		if err = os.Remove(path + ".previous"); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err = os.Rename(path, path+".previous"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	encodeErr := json.NewEncoder(f).Encode(record)
	closeErr := f.Close()
	if encodeErr != nil {
		return encodeErr
	}
	return closeErr
}

var _ io.Writer = (*stderrTap)(nil)
