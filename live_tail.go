package codextps

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"time"
)

type liveTailSeed struct {
	start          int64
	previous       *liveTokens
	latest         time.Time
	oversized      bool
	serviceTier    string
	serviceTierAt  time.Time
	hasServiceTier bool
}

// Locate complete turn boundaries backwards, but replay their events forwards.
// The preceding cumulative checkpoint seeds deduplication without being counted.
func locateLiveTail(file *os.File, size int64, window int) (liveTailSeed, error) {
	return locateLiveTailSince(file, size, window, time.Time{})
}

func locateLiveTailSince(file *os.File, size int64, window int, since time.Time) (liveTailSeed, error) {
	seed := liveTailSeed{}
	position := size
	carry := []byte{}
	discardTail := true // final JSONL fragment must wait for its newline
	seen := map[string]bool{}
	selected := false
	visit := func(line []byte, offset int64) bool {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			return false
		}
		if seed.latest.IsZero() {
			var row struct {
				Timestamp string `json:"timestamp"`
			}
			if json.Unmarshal(line, &row) == nil {
				seed.latest, _ = time.Parse(time.RFC3339Nano, row.Timestamp)
			}
		}
		if selected && offset < seed.start && !seed.hasServiceTier && bytes.Contains(line, []byte("\"thread_settings_applied\"")) {
			var row struct {
				Type      string `json:"type"`
				Timestamp string `json:"timestamp"`
				Payload   struct {
					Type           string         `json:"type"`
					ThreadSettings map[string]any `json:"thread_settings"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &row) == nil && row.Type == "event_msg" && row.Payload.Type == "thread_settings_applied" {
				if at, err := time.Parse(time.RFC3339Nano, row.Timestamp); err == nil {
					seed.serviceTier = textField(row.Payload.ThreadSettings, "service_tier")
					seed.serviceTierAt = at
					seed.hasServiceTier = true
				}
			}
		}
		if selected && offset < seed.start && bytes.Contains(line, []byte("\"token_count\"")) {
			var row map[string]any
			d := json.NewDecoder(bytes.NewReader(line))
			d.UseNumber()
			if d.Decode(&row) == nil && textField(row, "type") == "event_msg" {
				p, _ := row["payload"].(map[string]any)
				info, _ := p["info"].(map[string]any)
				total, _ := info["total_token_usage"].(map[string]any)
				if textField(p, "type") == "token_count" && total != nil {
					previous := tokensFrom(total)
					seed.previous = &previous
					return true
				}
			}
		}
		if !bytes.Contains(line, []byte("\"task_started\"")) {
			return false
		}
		var row struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Payload   struct {
				Type   string `json:"type"`
				TurnID string `json:"turn_id"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &row) != nil || row.Type != "event_msg" || row.Payload.Type != "task_started" {
			return false
		}
		id := row.Payload.TurnID
		if id == "" { // Legacy boundaries without IDs require the original full replay.
			selected = false
			seen = map[string]bool{}
			return false
		}
		if !selected && !seen[id] {
			seen[id] = true
			at, err := time.Parse(time.RFC3339Nano, row.Timestamp)
			covered := since.IsZero() || (err == nil && !at.After(since))
			if len(seen) >= window && covered {
				seed.start = offset
				selected = true
			}
		}
		return false
	}
	for position > 0 {
		length := min(position, int64(1024*1024))
		base := position - length
		block := make([]byte, length)
		if _, err := file.ReadAt(block, base); err != nil && err != io.EOF {
			return seed, err
		}
		data := append(block, carry...)
		parts := bytes.Split(data, []byte{'\n'})
		skipLast := discardTail
		if len(parts) > 1 {
			discardTail = false
		}
		end := len(data)
		for i := len(parts) - 1; i >= 1; i-- {
			start := end - len(parts[i])
			end = start - 1
			if skipLast && i == len(parts)-1 {
				continue
			}
			if len(parts[i]) > liveMaxLine {
				seed.oversized = true
				continue
			}
			if visit(parts[i], base+int64(start)) {
				return seed, nil
			}
		}
		if base == 0 {
			if !discardTail {
				visit(parts[0], 0)
			}
			// No earlier checkpoint: full replay preserves legacy/unassigned records.
			seed.start = 0
			seed.previous = nil
			seed.serviceTier = ""
			seed.hasServiceTier = false
			return seed, nil
		}
		if len(parts[0]) > liveMaxLine {
			seed.oversized = true
			carry = nil
			discardTail = true
		} else {
			carry = append([]byte{}, parts[0]...)
		}
		position = base
	}
	return seed, nil
}

func (f *liveFile) initialize(file *os.File, info os.FileInfo) error {
	seed, err := locateLiveTailSince(file, info.Size(), f.wantedTurns, f.wantedSince)
	if err != nil {
		return err
	}
	f.offset = seed.start
	f.hasEarlier = seed.start > 0
	f.loadedWindow = f.wantedTurns
	f.loadedSince = f.wantedSince
	if seed.start > 0 {
		// Only bootstrap metadata from the head; do not count older head responses.
		head := make([]byte, min(info.Size(), int64(256*1024)))
		n, _ := file.ReadAt(head, 0)
		for _, line := range bytes.SplitAfter(head[:n], []byte{'\n'}) {
			if len(line) == 0 || line[len(line)-1] != '\n' {
				break
			}
			var row map[string]any
			if json.Unmarshal(line, &row) != nil {
				continue
			}
			p, _ := row["payload"].(map[string]any)
			kind := textField(row, "type")
			if kind == "session_meta" {
				f.session.consume(line, 1)
			}
			if (kind == "event_msg" && textField(p, "type") == "user_message") || (kind == "response_item" && textField(p, "role") == "user") {
				f.session.setTitle(p)
				if f.session.Title != "" {
					break
				}
			}
		}
		f.session.lastTotal = seed.previous
		if seed.hasServiceTier {
			f.session.applyServiceTier(seed.serviceTier, seed.serviceTierAt)
		}
	}
	if !seed.latest.IsZero() {
		f.session.UpdatedAt = seed.latest
	}
	if seed.oversized {
		f.session.warn("存在超过 16 MiB 的日志行，定位最新轮次时已跳过该行")
	}
	f.initialized = true
	return nil
}

func (f *liveFile) requestWindow(window int) {
	f.wantedTurns = window
	if f.backfill != nil && len(f.session.Turns) >= window && !f.rangeMissing(f.wantedSince) {
		f.backfill = nil
	}
}
func (f *liveFile) requestSince(since time.Time) {
	f.wantedSince = since
	f.requestWindow(f.wantedTurns)
}
func (f *liveFile) rangeMissing(since time.Time) bool {
	if since.IsZero() || !f.hasEarlier || (!f.loadedSince.IsZero() && !f.loadedSince.After(since)) {
		return false
	}
	if len(f.session.Turns) > 0 {
		at := f.session.Turns[0].StartedAt
		if !at.IsZero() && !at.After(since) {
			return false
		}
	}
	return true
}
func (f *liveFile) historyPending(window int) bool {
	return f.hasEarlier && len(f.session.Turns) < window && (f.backfill != nil || window > f.loadedWindow)
}
func (f *liveFile) readLatest() error {
	if err := f.read(); err != nil {
		return err
	}
	if f.offset < f.size {
		return nil
	}
	needTurns := f.hasEarlier && f.wantedTurns > f.loadedWindow && len(f.session.Turns) < f.wantedTurns
	if f.backfill == nil && (needTurns || f.rangeMissing(f.wantedSince)) {
		f.backfill = newLiveFile(f.path)
		f.backfill.wantedTurns = f.wantedTurns
		f.backfill.wantedSince = f.wantedSince
	}
	if f.backfill != nil {
		if err := f.backfill.read(); err != nil {
			return err
		}
		if f.backfill.offset >= f.backfill.size {
			wanted, since, title := f.wantedTurns, f.wantedSince, f.session.Title
			*f = *f.backfill
			f.wantedTurns = wanted
			f.wantedSince = since
			if title != "" {
				f.session.Title = title
			}
		}
	}
	return nil
}
