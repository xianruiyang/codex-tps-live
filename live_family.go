package codextps

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"sort"
	"time"
)

const liveFamilyAllTurns = int(^uint(0) >> 1)

// The catalogue reads identities only. Full usage is loaded by the existing
// rollout reader, which excludes imported parent histories and deduplicates IDs.
type liveFamilyFile struct {
	ID, ParentID string
	Info         os.FileInfo
	Archived     bool
	Unreadable   bool
}

type liveFamilyRequest struct {
	Window int
	At     time.Time
}

type liveFamilyMember struct {
	ID            string          `json:"id"`
	ParentID      string          `json:"parent_id,omitempty"`
	Title         string          `json:"title"`
	AgentNickname string          `json:"agent_nickname,omitempty"`
	AgentPath     string          `json:"agent_path,omitempty"`
	Model         string          `json:"model"`
	Archived      bool            `json:"archived"`
	Available     bool            `json:"available"`
	Depth         int             `json:"depth"`
	Turns         int             `json:"turns"`
	Responses     int             `json:"responses"`
	Tokens        liveTokens      `json:"tokens"`
	TotalTokens   int64           `json:"total_tokens"`
	Credits       liveCreditUsage `json:"credits"`
	TokenShare    *float64        `json:"token_share_percent"`
	CreditShare   *float64        `json:"credit_share_percent"`
	Pending       bool            `json:"pending"`
}

type liveFamilyUsage struct {
	RootID      string             `json:"root_id"`
	SelectedID  string             `json:"selected_id"`
	Scope       string             `json:"scope"`
	Window      int                `json:"window"`
	Members     []liveFamilyMember `json:"members"`
	Tokens      liveTokens         `json:"tokens"`
	TotalTokens int64              `json:"total_tokens"`
	Credits     liveCreditUsage    `json:"credits"`
	Responses   int                `json:"responses"`
	Pending     bool               `json:"pending"`
	Warnings    []string           `json:"warnings"`
	CreditRules liveCreditPolicy   `json:"credit_rules"`
}

func readLiveFamilyFile(path string, info os.FileInfo, archived bool) liveFamilyFile {
	meta := liveFamilyFile{Info: info, Archived: archived, Unreadable: true}
	file, err := os.Open(path)
	if err != nil {
		return meta
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, 256*1024))
	scanner.Buffer(make([]byte, 4096), 256*1024)
	for scanner.Scan() {
		var row struct {
			Type    string `json:"type"`
			Payload struct {
				ID       string          `json:"id"`
				ParentID string          `json:"parent_thread_id"`
				Source   json.RawMessage `json:"source"`
			} `json:"payload"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) == nil && row.Type == "session_meta" && row.Payload.ID != "" {
			meta.ID, meta.ParentID = row.Payload.ID, row.Payload.ParentID
			if meta.ParentID == "" {
				var source struct {
					Subagent struct {
						Spawn struct {
							ParentID string `json:"parent_thread_id"`
						} `json:"thread_spawn"`
					} `json:"subagent"`
				}
				if json.Unmarshal(row.Payload.Source, &source) == nil {
					meta.ParentID = source.Subagent.Spawn.ParentID
				}
			}
			meta.Unreadable = false
			break // Later session_meta rows may describe inherited parent history.
		}
	}
	return meta
}

func (m *liveMonitor) familyParents(sessions map[string]*liveFile) map[string]string {
	parents := map[string]string{}
	chosen := map[string]liveFamilyFile{}
	paths := map[string]string{}
	for path, meta := range m.familyFiles {
		if meta.ID == "" {
			continue
		}
		old, ok := chosen[meta.ID]
		if !ok || meta.Info.ModTime().After(old.Info.ModTime()) || (meta.Info.ModTime().Equal(old.Info.ModTime()) && ((old.Archived && !meta.Archived) || (old.Archived == meta.Archived && path < paths[meta.ID]))) {
			chosen[meta.ID], paths[meta.ID] = meta, path
			parents[meta.ID] = meta.ParentID
		}
	}
	for id, f := range sessions {
		parents[id] = f.session.ParentID
	}
	return parents
}

// Return the recorded root and every descendant exactly once, including nested
// and archived agents. Missing ancestors remain explicit instead of relabeling
// an orphaned child as a main agent.
func liveFamilyTree(id string, parents map[string]string) (string, map[string]int, bool) {
	root, seen, broken := id, map[string]bool{}, false
	for parents[root] != "" {
		if seen[root] {
			broken = true
			break
		}
		seen[root] = true
		root = parents[root]
	}
	if _, exists := parents[root]; !exists {
		broken = true
	}
	children := map[string][]string{}
	for child, parent := range parents {
		if parent != "" {
			children[parent] = append(children[parent], child)
		}
	}
	depths, queue := map[string]int{root: 0}, []string{root}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, child := range children[parent] {
			if _, exists := depths[child]; !exists {
				depths[child] = depths[parent] + 1
				queue = append(queue, child)
			}
		}
	}
	return root, depths, broken
}

func (m *liveMonitor) updateFamilyScope(now time.Time) {
	if m.familyViews == nil {
		return
	}
	for id, request := range m.familyViews {
		if now.Sub(request.At) > 20*time.Second {
			delete(m.familyViews, id)
		}
	}
	if len(m.familyViews) == 0 {
		for _, f := range m.files {
			if f.familyTurns != 0 {
				f.familyTurns = 0
				f.requestWindow(f.windowTurns)
			}
		}
		m.familyViews = nil
		return
	}
	for _, f := range m.files {
		f.familyTurns = 0
	}
	parents := m.familyParents(m.sessions())
	for id, request := range m.familyViews {
		_, members, _ := liveFamilyTree(id, parents)
		for _, f := range m.files {
			if _, exists := members[f.session.ID]; exists {
				f.familyTurns = max(f.familyTurns, request.Window)
			}
		}
	}
	for _, f := range m.files {
		f.requestWindow(f.windowTurns)
	}
}

func (m *liveMonitor) hasActiveFamilyViews(now time.Time) bool {
	for _, request := range m.familyViews {
		if now.Sub(request.At) <= 20*time.Second {
			return true
		}
	}
	return false
}

func (m *liveMonitor) family(id string, window int, all bool, rules liveQuotaRules) (liveFamilyUsage, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sessions := m.sessions()
	if sessions[id] == nil {
		return liveFamilyUsage{}, false
	}
	if m.familyFiles == nil {
		m.familyFiles = map[string]liveFamilyFile{}
	}
	if m.familyViews == nil {
		m.familyViews = map[string]liveFamilyRequest{}
	}
	if !m.hasActiveFamilyViews(time.Now()) {
		m.familyReady = false
		m.discovered = time.Time{} // Discover older relatives on the next background poll.
	}
	requested := window
	if all {
		requested = liveFamilyAllTurns
	}
	m.familyViews[id] = liveFamilyRequest{Window: requested, At: time.Now()}
	m.updateFamilyScope(time.Now())
	parents := m.familyParents(sessions)
	root, members, broken := liveFamilyTree(id, parents)
	out := liveFamilyUsage{RootID: root, SelectedID: id, Scope: "window", Window: window, Members: []liveFamilyMember{}, Warnings: []string{}, Pending: !m.familyReady, CreditRules: creditPolicy(rules)}
	if all {
		out.Scope, out.Window = "all", 0
	}
	if broken {
		out.Warnings = append(out.Warnings, "父代理日志缺失或关联成环，当前汇总可能不完整。")
	}
	for memberID, depth := range members {
		row := liveFamilyMember{ID: memberID, ParentID: parents[memberID], Depth: depth, Title: "等待加载代理日志", Pending: true}
		if _, exists := parents[memberID]; !exists && m.familyReady {
			row.Title, row.Pending = "代理日志缺失", false
		}
		if f := sessions[memberID]; f != nil {
			s := f.session
			row.Available = true
			row.Title, row.Model, row.Archived = s.Title, s.model, f.archived
			if name := m.names.entries[s.ID].Name; name != "" {
				row.Title = name
			}
			row.AgentNickname, row.AgentPath = s.AgentNickname, s.AgentPath
			start := 0
			if !all {
				start = windowStart(s, window)
			}
			for _, turn := range s.Turns[start:] {
				row.Tokens.add(turn.Metrics.Tokens)
				row.Responses += turn.Metrics.Responses
				row.Credits.add(turnCredits(s, turn, sessions, rules))
			}
			row.Turns = len(s.Turns) - start
			row.Pending = f.offset < f.size || (all && f.hasEarlier) || (!all && f.historyPending(window)) || len(f.pending) > 0 || f.dropping
		}
		row.TotalTokens = row.Tokens.Input + row.Tokens.Output
		out.Tokens.add(row.Tokens)
		out.TotalTokens += row.TotalTokens
		out.Responses += row.Responses
		out.Credits.add(row.Credits)
		out.Pending = out.Pending || row.Pending
		out.Members = append(out.Members, row)
	}
	for _, meta := range m.familyFiles {
		if meta.Unreadable {
			out.Warnings = append(out.Warnings, "部分日志的头部关联信息无法读取，可能缺少相关代理；仅汇总可识别的本机记录。")
			break
		}
	}
	for i := range out.Members {
		row := &out.Members[i]
		row.TokenShare = ratio(float64(row.TotalTokens)*100, float64(out.TotalTokens))
		if out.Credits.Value != nil {
			if row.Credits.Value != nil {
				row.CreditShare = ratio(*row.Credits.Value*100, *out.Credits.Value)
			} else if row.Responses == 0 {
				row.CreditShare = ratio(0, *out.Credits.Value)
			}
		}
	}
	sort.Slice(out.Members, func(i, j int) bool {
		a, b := out.Members[i], out.Members[j]
		if a.Depth != b.Depth {
			return a.Depth < b.Depth
		}
		if a.TotalTokens != b.TotalTokens {
			return a.TotalTokens > b.TotalTokens
		}
		return a.ID < b.ID
	})
	if !out.Pending {
		// A completed one-shot calculation has no ongoing read demand.
		delete(m.familyViews, id)
		m.updateFamilyScope(time.Now())
	}
	return out, true
}
