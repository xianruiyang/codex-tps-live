package codextps

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const liveReadBudget = 8 * 1024 * 1024
const liveMaxLine = 16 * 1024 * 1024

type liveFile struct {
	path         string
	archived     bool
	offset, size int64
	line         int
	pending      []byte
	dropping     bool
	info         os.FileInfo
	checkpoint   []byte
	modified     time.Time
	session      *liveSession
	initialized  bool
	hasEarlier   bool
	wantedTurns  int
	loadedWindow int
	wantedSince  time.Time
	loadedSince  time.Time
	backfill     *liveFile
}

func liveID(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if len(name) >= 36 {
		id := name[len(name)-36:]
		if sessionIDPattern.MatchString(id) {
			return id
		}
	}
	return name
}
func newLiveFile(path string) *liveFile {
	return &liveFile{path: path, session: newLiveSession(liveID(path)), wantedTurns: 10}
}

func (f *liveFile) setLocation(path string, archived bool) {
	f.path, f.archived = path, archived
	if f.backfill != nil {
		f.backfill.setLocation(path, archived)
	}
}

func (f *liveFile) read() error {
	file, err := os.Open(f.path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	reset := f.info != nil && (!os.SameFile(f.info, info) || info.Size() < f.offset)
	if !reset && f.offset > 0 && len(f.checkpoint) > 0 {
		check := make([]byte, len(f.checkpoint))
		n, _ := file.ReadAt(check, f.offset-int64(len(check)))
		reset = n != len(check) || !bytes.Equal(check, f.checkpoint)
	}
	if !reset && f.info != nil && info.Size() == f.offset && !info.ModTime().Equal(f.modified) {
		reset = true
	}
	if reset {
		wanted, since, archived := f.wantedTurns, f.wantedSince, f.archived
		*f = *newLiveFile(f.path)
		f.archived = archived
		f.wantedTurns = wanted
		f.wantedSince = since
		f.session.warn("日志已重建或截断，统计已从文件重新建立")
	}
	f.info, f.size, f.modified = info, info.Size(), info.ModTime()
	if !f.initialized {
		if err := f.initialize(file, info); err != nil {
			return err
		}
	}
	if f.offset >= f.size {
		return nil
	}
	if _, err = file.Seek(f.offset, io.SeekStart); err != nil {
		return err
	}
	buffer := make([]byte, 256*1024)
	remaining := int64(liveReadBudget)
	for remaining > 0 {
		readSize := min(int64(len(buffer)), remaining)
		n, readErr := file.Read(buffer[:readSize])
		f.offset += int64(n)
		remaining -= int64(n)
		chunk := buffer[:n]
		for len(chunk) > 0 {
			end := bytes.IndexByte(chunk, '\n')
			take := len(chunk)
			if end >= 0 {
				take = end + 1
			}
			if !f.dropping {
				if len(f.pending)+take > liveMaxLine {
					f.pending = nil
					f.dropping = true
					f.session.warn("存在超过 16 MiB 的日志行，已跳过该行以限制内存占用")
				} else {
					f.pending = append(f.pending, chunk[:take]...)
				}
			}
			chunk = chunk[take:]
			if end >= 0 {
				f.line++
				if !f.dropping {
					line := bytes.TrimSpace(f.pending)
					if len(line) > 0 {
						f.session.consume(line, f.line)
					}
				}
				if cap(f.pending) > 1024*1024 {
					f.pending = nil
				} else {
					f.pending = f.pending[:0]
				}
				f.dropping = false
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				return readErr
			}
			break
		}
		if n == 0 {
			break
		}
	}
	if f.offset > 0 {
		f.checkpoint = make([]byte, min(int64(64), f.offset))
		_, _ = file.ReadAt(f.checkpoint, f.offset-int64(len(f.checkpoint)))
	}
	return nil
}

type liveMonitor struct {
	mu             sync.RWMutex
	home           string
	days           int
	showArchived   bool
	interval       time.Duration
	files          map[string]*liveFile
	errors         []string
	updated        time.Time
	discovered     time.Time
	trendSince     time.Time
	trendViews     map[string]liveTrendRequest
	quota          *liveQuotaHistory
	names          liveSessionNames
	timingReader   *liveTimingReader
	timings        liveTimingIndex
	timingError    string
	captureReader  *liveCaptureReader
	captureStatus  liveCaptureStatus
	captureRecords map[string]liveCapturedTiming
}

func newLiveMonitor(home string, days int, showArchived bool, interval time.Duration) *liveMonitor {
	return &liveMonitor{home: home, days: days, showArchived: showArchived, interval: interval, files: map[string]*liveFile{}, errors: []string{}}
}
func (m *liveMonitor) discover(now time.Time) {
	// Collection is independent of the conversation list's display preference.
	roots := []string{"sessions", "archived_sessions"}
	type discoveredFile struct {
		info     os.FileInfo
		archived bool
	}
	seen := map[string]discoveredFile{}
	known := map[string][]*liveFile{}
	for _, f := range m.files {
		id := liveID(f.path)
		known[id] = append(known[id], f)
	}
	cutoff := now.AddDate(0, 0, -m.days)
	if !m.trendSince.IsZero() && m.trendSince.Before(cutoff) {
		cutoff = m.trendSince
	}
	for _, root := range roots {
		path := filepath.Join(m.home, root)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(path, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				m.addError("无法读取日志目录: " + path + " (" + err.Error() + ")")
				if entry != nil && entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".jsonl") {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return nil
			}
			if m.days > 0 && info.ModTime().Before(cutoff) && len(known[liveID(path)]) == 0 {
				return nil
			}
			seen[path] = discoveredFile{info: info, archived: root == "archived_sessions"}
			return nil
		})
		if err != nil {
			m.addError(err.Error())
		}
	}
	for path, found := range seen {
		f := m.files[path]
		if f == nil {
			// A rename keeps the same file identity. Retain tail checkpoints and
			// in-flight backfill instead of briefly removing the conversation.
			for _, old := range known[liveID(path)] {
				if _, exists := seen[old.path]; !exists && old.info != nil && os.SameFile(old.info, found.info) {
					f = old
					delete(m.files, old.path)
					break
				}
			}
			if f == nil {
				f = newLiveFile(path)
				f.modified = found.info.ModTime()
			}
			m.files[path] = f
		}
		f.setLocation(path, found.archived)
	}
	for path := range m.files {
		if _, exists := seen[path]; !exists {
			delete(m.files, path)
		}
	}
	if _, err := os.Stat(m.home); err != nil {
		m.addError("Codex 日志目录不可用: " + m.home)
	}
	m.discovered = now
}
func (m *liveMonitor) addError(s string) {
	for _, old := range m.errors {
		if old == s {
			return
		}
	}
	if len(m.errors) < 12 {
		m.errors = append(m.errors, s)
	}
}
func (m *liveMonitor) poll() {
	m.mu.Lock()
	now := time.Now()
	m.errors = []string{}
	if m.timingError != "" {
		m.addError(m.timingError)
	}
	m.refreshSessionNames()
	m.updateTrendScope(now)
	if m.discovered.IsZero() || now.Sub(m.discovered) >= 5*time.Second {
		m.discover(now)
	}
	files := make([]*liveFile, 0, len(m.files))
	for _, f := range m.files {
		f.requestSince(m.trendSince)
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].modified.Equal(files[j].modified) {
			return files[i].path < files[j].path
		}
		return files[i].modified.After(files[j].modified)
	})
	m.mu.Unlock()
	for _, f := range files {
		m.mu.Lock()
		if err := f.readLatest(); err != nil {
			m.addError("无法读取 " + filepath.Base(f.path) + ": " + err.Error())
		}
		m.mu.Unlock()
	}
	m.mu.Lock()
	m.collectQuota(files)
	m.updated = time.Now()
	m.mu.Unlock()
	m.pollTiming(time.Now())
	m.pollCapture(time.Now())
}

// A duplicate/moved rollout represents the same conversation, never additive usage.
func (m *liveMonitor) sessions() map[string]*liveFile {
	byID := map[string]*liveFile{}
	for _, f := range m.files {
		if !f.initialized {
			continue
		}
		old := byID[f.session.ID]
		if old == nil || f.modified.After(old.modified) || (f.modified.Equal(old.modified) && ((old.archived && !f.archived) || (f.archived == old.archived && f.path < old.path))) {
			byID[f.session.ID] = f
		}
	}
	return byID
}

type liveSessionView struct {
	ID                string          `json:"id"`
	Archived          bool            `json:"archived"`
	Title             string          `json:"title"`
	CWD               string          `json:"cwd"`
	Model             string          `json:"model"`
	Effort            string          `json:"effort"`
	ServiceTier       string          `json:"service_tier"`
	ServiceTierSource string          `json:"service_tier_source,omitempty"`
	ParentID          string          `json:"parent_id,omitempty"`
	AgentNickname     string          `json:"agent_nickname,omitempty"`
	AgentRole         string          `json:"agent_role,omitempty"`
	AgentPath         string          `json:"agent_path,omitempty"`
	Status            string          `json:"status"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	LastResponseAt    time.Time       `json:"last_response_at"`
	Turns             int             `json:"turns"`
	TotalTurns        int             `json:"total_turns"`
	TotalKnown        bool            `json:"total_known"`
	HistoryPending    bool            `json:"history_pending"`
	Window            int             `json:"window"`
	Metrics           liveMetrics     `json:"metrics"`
	TPS               liveRates       `json:"tps"`
	Rate30            liveTokens      `json:"recorded_tokens_30s"`
	Warnings          []string        `json:"warnings"`
	Indexing          bool            `json:"indexing"`
	PendingLine       bool            `json:"pending_line"`
	LatestResponse    *liveResponse   `json:"latest_response"`
	Credits           liveCreditUsage `json:"credits"`
	TimingPending     bool            `json:"timing_pending"`
}

func windowStart(s *liveSession, window int) int { return max(0, len(s.Turns)-window) }

func sessionView(f *liveFile, now time.Time, window int) liveSessionView {
	s := f.session
	status := "idle"
	if len(s.Turns) > 0 {
		t := s.Turns[len(s.Turns)-1]
		if t.Status == "active" {
			status = "active"
		}
	}
	start := windowStart(s, window)
	metrics := liveMetrics{}
	var latest *liveResponse
	for _, turn := range s.Turns[start:] {
		metrics.merge(turn.Metrics)
		for ri := len(turn.Responses) - 1; ri >= 0; ri-- {
			r := turn.Responses[ri]
			if latest == nil || r.RecordedAt.After(latest.RecordedAt) {
				latest = &r
			}
			break
		}
	}
	v := liveSessionView{ID: s.ID, Title: s.Title, CWD: s.CWD, Model: s.model, Effort: s.effort, ParentID: s.ParentID, Status: status, UpdatedAt: s.UpdatedAt, Turns: len(s.Turns) - start, TotalTurns: len(s.Turns), Window: window, Metrics: metrics, TPS: metrics.rates(), Warnings: s.Warnings, Indexing: f.offset < f.size, PendingLine: len(f.pending) > 0 || f.dropping, LatestResponse: latest}
	v.CreatedAt, v.LastResponseAt = s.CreatedAt, s.LastResponseAt
	v.Archived = f.archived
	v.TotalKnown = !f.hasEarlier
	v.AgentNickname, v.AgentRole = s.AgentNickname, s.AgentRole
	v.AgentPath = s.AgentPath
	v.ServiceTier = s.serviceTier
	if len(s.serviceTiers) > 0 {
		v.ServiceTierSource = "logged"
	}
	v.HistoryPending = f.historyPending(window)
	if v.Indexing {
		v.LatestResponse = nil
		v.ServiceTier = ""
		v.ServiceTierSource = ""
	}
	if v.Title == "" && s.ParentID != "" {
		v.Title = s.agentName()
	}
	if v.Title == "" {
		v.Title = "未命名对话"
	}
	cutoff := now.Add(-30 * time.Second)
	for ti := len(s.Turns) - 1; ti >= start; ti-- {
		responses := s.Turns[ti].Responses
		for ri := len(responses) - 1; ri >= 0; ri-- {
			r := responses[ri]
			if r.RecordedAt.Before(cutoff) {
				return v
			}
			v.Rate30.add(r.Tokens)
		}
	}
	return v
}

type liveSnapshot struct {
	Home                  string            `json:"codex_home"`
	Days                  int               `json:"days"`
	IntervalMS            int64             `json:"interval_ms"`
	UpdatedAt             time.Time         `json:"updated_at"`
	Errors                []string          `json:"errors"`
	Sessions              []liveSessionView `json:"sessions"`
	CreditRules           liveCreditPolicy  `json:"credit_rules"`
	ShowArchivedByDefault bool              `json:"show_archived_by_default"`
	CaptureStatus         liveCaptureStatus `json:"capture_status"`
}

func (m *liveMonitor) snapshot(window int, policies ...liveQuotaRules) liveSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.files {
		f.requestWindow(window)
	}
	rules := creditRules(policies)
	result := liveSnapshot{Home: m.home, Days: m.days, IntervalMS: m.interval.Milliseconds(), UpdatedAt: m.updated, Errors: m.errors, Sessions: []liveSessionView{}, CreditRules: creditPolicy(rules), ShowArchivedByDefault: m.showArchived}
	result.CaptureStatus = m.captureStatus
	sessions := m.sessions()
	for _, f := range sessions {
		result.Sessions = append(result.Sessions, m.namedSessionView(f, time.Now(), window, sessions, rules))
	}
	sort.Slice(result.Sessions, func(i, j int) bool {
		a, b := result.Sessions[i], result.Sessions[j]
		at, bt := a.LastResponseAt, b.LastResponseAt
		if at.IsZero() {
			at = a.CreatedAt
		}
		if bt.IsZero() {
			bt = b.CreatedAt
		}
		if at.Equal(bt) {
			return a.ID < b.ID
		}
		return at.After(bt)
	})
	return result
}

type liveTurnView struct {
	*liveTurn
	TPS      liveRates       `json:"tps"`
	Position int             `json:"position"`
	Credits  liveCreditUsage `json:"credits"`
	TTFT     *liveFirstToken `json:"ttft,omitempty"`
}
type liveDetail struct {
	Session     liveSessionView  `json:"session"`
	Turns       []liveTurnView   `json:"turns"`
	Total       int              `json:"total"`
	Offset      int              `json:"offset"`
	CreditRules liveCreditPolicy `json:"credit_rules"`
}

func (m *liveMonitor) detail(id string, offset, limit, window int, policies ...liveQuotaRules) (liveDetail, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sessions := m.sessions()
	f := sessions[id]
	if f == nil {
		return liveDetail{}, false
	}
	s := f.session
	f.requestWindow(window)
	start := windowStart(s, window)
	rules := creditRules(policies)
	out := liveDetail{Session: m.namedSessionView(f, time.Now(), window, sessions, rules), Turns: []liveTurnView{}, Total: len(s.Turns) - start, Offset: offset, CreditRules: creditPolicy(rules)}
	for i := len(s.Turns) - 1 - offset; i >= start && len(out.Turns) < limit; i-- {
		copy := *s.Turns[i]
		copy.Metrics = m.timedTurn(s, s.Turns[i])
		if copy.ServiceTierSource == "" {
			copy.ServiceTier, copy.ServiceTierSource = inheritedServiceTier(s, sessions, copy.StartedAt, false)
		}
		if s.ParentID != "" && (copy.Title == "" || copy.Title == "等待用户轮次信息") {
			if name := s.agentName(); name != "" {
				copy.Title = name
			}
		}
		var first *liveFirstToken
		if len(copy.Responses) > 0 {
			response := copy.Responses[0]
			m.enrichTiming(s, s.Turns[i], &response)
			first = response.TTFT
		}
		out.Turns = append(out.Turns, liveTurnView{liveTurn: &copy, TPS: copy.Metrics.rates(), Position: len(s.Turns) - i, Credits: turnCredits(s, s.Turns[i], sessions, rules), TTFT: first})
	}
	return out, true
}
func (m *liveMonitor) responses(id, turnID string, policies ...liveQuotaRules) ([]liveResponse, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sessions := m.sessions()
	f := sessions[id]
	if f == nil {
		return nil, false
	}
	t := f.session.turnsByID[turnID]
	if t == nil {
		return nil, false
	}
	rows := append([]liveResponse{}, t.Responses...)
	rules := creditRules(policies)
	for i := range rows {
		m.enrichTiming(f.session, t, &rows[i])
		fillResponseServiceTier(f.session, &rows[i], sessions)
		priceResponse(&rows[i], rules)
	}
	return rows, true
}
func (m *liveMonitor) allResponses(id string, window int) ([]liveResponse, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.sessions()[id]
	if f == nil {
		return nil, false
	}
	f.requestWindow(window)
	result := []liveResponse{}
	for _, t := range f.session.Turns[windowStart(f.session, window):] {
		for _, recorded := range t.Responses {
			r := recorded
			m.enrichTiming(f.session, t, &r)
			result = append(result, r)
		}
	}
	return result, true
}
