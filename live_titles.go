package codextps

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type liveSessionName struct {
	ID        string    `json:"id"`
	Name      string    `json:"thread_name"`
	UpdatedAt time.Time `json:"updated_at"`
}

type liveSessionNames struct {
	info    os.FileInfo
	entries map[string]liveSessionName
}

// Called under the monitor lock. Codex appends names and later renames here;
// unchanged files need only a stat, and replacement/truncation rebuilds the map.
func (m *liveMonitor) refreshSessionNames() {
	path := filepath.Join(m.home, "session_index.jsonl")
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		m.names = liveSessionNames{}
		return
	}
	if err != nil {
		m.addError("无法读取对话名称索引: " + err.Error())
		return
	}
	previous := m.names.info
	if previous != nil && os.SameFile(previous, info) && previous.Size() == info.Size() && previous.ModTime().Equal(info.ModTime()) {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		m.addError("无法读取对话名称索引: " + err.Error())
		return
	}
	entries := map[string]liveSessionName{}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var entry liveSessionName
		if json.Unmarshal(line, &entry) != nil {
			// A final row may still be in flight; a later file change retries it.
			continue
		}
		entry.Name = strings.TrimSpace(entry.Name)
		if entry.ID == "" || entry.Name == "" {
			continue
		}
		if old, ok := entries[entry.ID]; ok && entry.UpdatedAt.Before(old.UpdatedAt) {
			continue
		}
		entries[entry.ID] = entry
	}
	m.names = liveSessionNames{info: info, entries: entries}
}

func (m *liveMonitor) namedSessionView(f *liveFile, now time.Time, window int, sessions map[string]*liveFile, rules liveQuotaRules) liveSessionView {
	v := tierSessionView(f, now, window, sessions, rules)
	v.TimingPending = m.timings.pending
	v.Metrics = liveMetrics{}
	for _, turn := range f.session.Turns[windowStart(f.session, window):] {
		v.Metrics.merge(m.timedTurn(f.session, turn))
	}
	v.TPS = v.Metrics.rates()
	if v.LatestResponse != nil {
		if turn := f.session.turnsByID[v.LatestResponse.TurnID]; turn != nil {
			m.enrichTiming(f.session, turn, v.LatestResponse)
		}
	}
	if name := m.names.entries[v.ID].Name; name != "" {
		v.Title = name
	}
	return v
}
