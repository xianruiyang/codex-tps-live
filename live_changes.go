package codextps

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Only reader checkpoints and metadata enter change detection, never response
// arrays or the poll timestamp. Idle polls must keep the same revision.
type liveViewFileStamp struct {
	Session                        *liveSession
	Offset, Size, Modified         int64
	LoadedSince                    time.Time
	LoadedWindow, Pending          int
	Initialized, Earlier, Backfill bool
	Archived, Dropping             bool
}

func viewFileStamp(f *liveFile) liveViewFileStamp {
	return liveViewFileStamp{f.session, f.offset, f.size, f.modified.UnixNano(), f.loadedSince, f.loadedWindow, len(f.pending), f.initialized, f.hasEarlier, f.backfill != nil, f.archived, f.dropping}
}

type liveViewTierStamp struct {
	Session  *liveSession
	Settings int
	Tier     string
	Indexing bool
}

func viewTierStamp(s *liveSession, sessions map[string]*liveFile) liveViewTierStamp {
	if root := liveTierRoot(s, sessions); root != nil {
		return liveViewTierStamp{root.session, len(root.session.serviceTiers), root.session.serviceTier, root.offset < root.size}
	}
	return liveViewTierStamp{}
}

type liveSessionCacheKey struct {
	File                          liveViewFileStamp
	Tier                          liveViewTierStamp
	Name                          liveSessionName
	Timing                        uint64
	Orphan                        string
	Window                        int
	Policy                        string
	Pending, Unavailable, History bool
}

type liveSessionCacheEntry struct {
	Key  liveSessionCacheKey
	View liveSessionView
}

func liveCreditCacheKey(rules liveQuotaRules) string {
	data, _ := json.Marshal(rules)
	return string(data)
}

func sessionRate30(s *liveSession, now time.Time, window int) liveTokens {
	var tokens liveTokens
	cutoff := now.Add(-30 * time.Second)
	for ti := len(s.Turns) - 1; ti >= windowStart(s, window); ti-- {
		responses := s.Turns[ti].Responses
		for ri := len(responses) - 1; ri >= 0; ri-- {
			if responses[ri].RecordedAt.Before(cutoff) {
				return tokens
			}
			tokens.add(responses[ri].Tokens)
		}
	}
	return tokens
}

func (m *liveMonitor) cachedSessionView(f *liveFile, now time.Time, window int, sessions map[string]*liveFile, rules liveQuotaRules, policy string) liveSessionView {
	if m.sessionViews == nil {
		m.sessionViews = map[string]liveSessionCacheEntry{}
	}
	key := liveSessionCacheKey{File: viewFileStamp(f), Tier: viewTierStamp(f.session, sessions), Name: m.names.entries[f.session.ID], Timing: m.timings.threadRevisions[f.session.ID], Orphan: m.timings.orphanStampBefore(f.session.UpdatedAt), Window: window, Policy: policy, Pending: m.timings.pending, Unavailable: m.timings.unavailable, History: f.historyPending(window)}
	entry, exists := m.sessionViews[f.session.ID]
	if !exists || entry.Key != key {
		entry = liveSessionCacheEntry{Key: key, View: m.namedSessionView(f, now, window, sessions, rules)}
		m.sessionViews[f.session.ID] = entry
	}
	view := entry.View
	// This small moving window must expire even without another token event.
	view.Rate30 = sessionRate30(f.session, now, window)
	return view
}

type liveChanges struct {
	Sessions string            `json:"sessions_revision"`
	Session  string            `json:"session_revision"`
	Timing   string            `json:"timing_revision"`
	Quota    string            `json:"quota_revision"`
	Time     time.Time         `json:"server_time"`
	Capture  liveCaptureStatus `json:"capture_status"`
}

func liveChangeHash(values ...any) string {
	h := fnv.New64a()
	for _, value := range values {
		fmt.Fprintf(h, "%v\x00", value)
	}
	return strconv.FormatUint(h.Sum64(), 36)
}

func (m *liveMonitor) changes(id, page, view string) liveChanges {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if m.changeEpoch == "" {
		m.changeEpoch = strconv.FormatInt(now.UnixNano(), 36)
	}
	// Lightweight polling keeps a visible chart's existing history lease alive.
	// A new or changed range still registers through its normal data endpoint.
	if view != "" && (page == "trends" || page == "quota") {
		key := view
		if page == "quota" {
			key = "quota/" + view
		}
		if request, exists := m.trendViews[key]; exists {
			request.seen = now
			m.trendViews[key] = request
		}
	}
	paths := make([]string, 0, len(m.files))
	for path := range m.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	h := fnv.New64a()
	pending := 0
	for _, path := range paths {
		f := m.files[path]
		fmt.Fprintf(h, "%s\x00%v\x00", path, viewFileStamp(f))
		if !f.initialized || f.offset < f.size || f.rangeMissing(m.trendSince) {
			pending++
		}
	}
	var nameSize, nameModified int64
	if m.names.info != nil {
		nameSize, nameModified = m.names.info.Size(), m.names.info.ModTime().UnixNano()
	}
	base := liveChangeHash(m.changeEpoch, h.Sum64(), m.names.revision, nameSize, nameModified, strings.Join(m.errors, "\n"), m.discovered.IsZero())
	timing := liveChangeHash(base, m.timings.revision, m.timings.pending, m.timings.unavailable)
	out := liveChanges{Sessions: timing, Session: "missing", Timing: timing, Quota: liveChangeHash(m.changeEpoch, pending, m.discovered.IsZero()), Time: now, Capture: m.captureStatus}
	if m.quota != nil {
		m.quota.advanceClock(now.Unix())
		out.Quota = liveChangeHash(m.changeEpoch, m.quota.revision, m.quota.clockRevision, len(m.quota.records), len(m.quota.waitingTier), m.quota.errorText, pending, m.discovered.IsZero())
	}
	sessions := m.sessions()
	if f := sessions[id]; f != nil {
		// Rate-window crossings are changes; wall-clock polling itself is not.
		rate := sessionRate30(f.session, now, len(f.session.Turns))
		out.Session = liveChangeHash(m.changeEpoch, viewFileStamp(f), viewTierStamp(f.session, sessions), m.names.entries[id], m.timings.threadRevisions[id], m.timings.orphanStampBefore(f.session.UpdatedAt), m.timings.pending, m.timings.unavailable, rate)
	}
	return out
}

// A new unscoped header after a conversation's last log event cannot change any
// of its recorded responses. Historical inserts and corrected observations can.
func (x *liveTimingIndex) orphanStampBefore(cutoff time.Time) string {
	parts := make([]string, 0, len(x.headers))
	for process, group := range x.headers {
		count := sort.Search(len(group), func(i int) bool { return group[i].at.After(cutoff) })
		if count > 0 {
			parts = append(parts, process+":"+strconv.Itoa(count))
		}
	}
	sort.Strings(parts)
	return liveChangeHash(x.orphanCorrections, parts)
}

// Preserve the full public snapshot by default. The sidebar explicitly requests
// only the response fields it displays, avoiding an unused response/metrics dump.
type liveSidebarResponse struct {
	TPS             liveRates `json:"tps"`
	Transport       string    `json:"transport"`
	TransportSource string    `json:"transport_source"`
	TransportNote   string    `json:"transport_note"`
}

type liveSidebarMetrics struct {
	Tokens     liveTokens           `json:"tokens"`
	Transports liveTransportSummary `json:"transports"`
}

type liveSidebarSession struct {
	liveSessionView
	Metrics        liveSidebarMetrics   `json:"metrics"`
	LatestResponse *liveSidebarResponse `json:"latest_response"`
}

func compactLiveSnapshot(snapshot liveSnapshot) any {
	rows := make([]liveSidebarSession, 0, len(snapshot.Sessions))
	for _, view := range snapshot.Sessions {
		row := liveSidebarSession{liveSessionView: view, Metrics: liveSidebarMetrics{Tokens: view.Metrics.Tokens, Transports: view.Metrics.Transports}}
		if r := view.LatestResponse; r != nil {
			row.LatestResponse = &liveSidebarResponse{TPS: r.TPS, Transport: r.Transport, TransportSource: r.TransportSource, TransportNote: r.TransportNote}
		}
		rows = append(rows, row)
	}
	return struct {
		liveSnapshot
		Sessions []liveSidebarSession `json:"sessions"`
	}{snapshot, rows}
}
