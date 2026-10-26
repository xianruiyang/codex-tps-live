package codextps

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// History contains only quota snapshots and usage counters, never chat text or credentials.
// Keys include the source conversation and checkpoint, so tail replay, moved files,
// and history backfill can update a record without counting its tokens again.
type liveQuotaWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int64   `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

type liveQuotaRecord struct {
	ID        string           `json:"id"`
	At        time.Time        `json:"at"`
	Bucket    string           `json:"bucket"`
	LimitName string           `json:"limit_name,omitempty"`
	PlanType  string           `json:"plan_type,omitempty"`
	Primary   *liveQuotaWindow `json:"primary,omitempty"`
	Secondary *liveQuotaWindow `json:"secondary,omitempty"`
	Balance   *float64         `json:"credits_balance,omitempty"`
	Usage     *liveQuotaUsage  `json:"usage,omitempty"`
	response  *liveResponse
	legacyID  string
}

type liveQuotaUsage struct {
	Model       string     `json:"model"`
	Effort      string     `json:"effort,omitempty"`
	ServiceTier string     `json:"service_tier"`
	Tokens      liveTokens `json:"tokens"`
}

func quotaUsageFrom(r *liveResponse) *liveQuotaUsage {
	return &liveQuotaUsage{Model: r.Model, Effort: r.Effort, ServiceTier: r.ServiceTier, Tokens: r.Tokens}
}

type liveQuotaHistory struct {
	path        string
	errorText   string
	records     map[string]liveQuotaRecord
	dirty       map[string]liveQuotaRecord
	flushedAt   time.Time
	waitingTier map[string]liveQuotaTierWait
}

type liveQuotaTierWait struct {
	parent string
	at     time.Time
}

func quotaNumber(v any) (float64, bool) {
	var n float64
	var err error
	switch x := v.(type) {
	case json.Number:
		n, err = x.Float64()
	case float64:
		n = x
	case string:
		n, err = strconv.ParseFloat(strings.TrimSpace(x), 64)
	default:
		return 0, false
	}
	return n, err == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
}

func parseQuotaWindow(raw any) *liveQuotaWindow {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	used, ok := quotaNumber(m["used_percent"])
	if !ok || used < 0 {
		return nil
	}
	return &liveQuotaWindow{UsedPercent: used, WindowMinutes: integer(m, "window_minutes"), ResetsAt: integer(m, "resets_at")}
}

func (s *liveSession) captureQuota(p map[string]any, at time.Time, previous int) {
	limits, _ := p["rate_limits"].(map[string]any)
	record := liveQuotaRecord{At: at, Bucket: textField(limits, "limit_id"), LimitName: textField(limits, "limit_name"), PlanType: textField(limits, "plan_type"), Primary: parseQuotaWindow(limits["primary"]), Secondary: parseQuotaWindow(limits["secondary"])}
	if credits, ok := limits["credits"].(map[string]any); ok {
		if n, valid := quotaNumber(credits["balance"]); valid && n >= 0 {
			record.Balance = &n
		}
	}
	if s.Metrics.Responses > previous && s.current != nil && len(s.current.Responses) > 0 {
		r := s.current.Responses[len(s.current.Responses)-1]
		record.response = &r
		record.Usage = quotaUsageFrom(&r)
	}
	if record.Primary == nil && record.Secondary == nil && record.Balance == nil && record.Usage == nil {
		return
	}
	// Missing limit IDs are a separate, explicit bucket, never silently mixed with codex.
	if record.Bucket == "" {
		record.Bucket = "unidentified"
	}
	info, _ := p["info"].(map[string]any)
	total, _ := info["total_token_usage"].(map[string]any)
	checkpoint, _ := json.Marshal(total)
	checkpointID := func(id string) string {
		key := sha256.Sum256([]byte(id + "\x00" + at.Format(time.RFC3339Nano) + "\x00" + record.Bucket + "\x00" + string(checkpoint)))
		return hex.EncodeToString(key[:16])
	}
	record.ID = checkpointID(s.ID)
	if s.legacyID != "" && s.legacyID != s.ID {
		record.legacyID = checkpointID(s.legacyID)
	}
	s.quotaPending = append(s.quotaPending, record)
}

func openLiveQuotaHistory(home, dataDir, profile string) *liveQuotaHistory {
	h := &liveQuotaHistory{records: map[string]liveQuotaRecord{}, dirty: map[string]liveQuotaRecord{}, waitingTier: map[string]liveQuotaTierWait{}}
	if dataDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			h.errorText = "无法定位本机数据目录，额度记录暂存内存: " + err.Error()
			return h
		}
		dataDir = filepath.Join(base, "CodexTPSLive")
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		h.errorText = "无法定位额度记录目录: " + err.Error()
		return h
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		h.errorText = "无法创建额度记录目录，暂存内存: " + err.Error()
		return h
	}
	keyHome := filepath.Clean(home)
	if os.PathSeparator == '\\' {
		keyHome = strings.ToLower(keyHome)
	}
	key := sha256.Sum256([]byte(keyHome + "\x00" + profile))
	h.path = filepath.Join(abs, "quota-"+hex.EncodeToString(key[:8])+".jsonl")
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		h.errorText = "无法读取额度历史，暂存内存: " + err.Error()
		return h
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	var offset int64
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr == io.EOF {
			if len(line) > 0 {
				var record liveQuotaRecord
				if err := json.Unmarshal(line, &record); err == nil && record.ID != "" && !record.At.IsZero() {
					h.records[record.ID] = record
					if _, err := f.Write([]byte{'\n'}); err != nil {
						h.errorText = "额度历史末行缺少换行，修复失败: " + err.Error()
					}
				} else {
					// Drop only an interrupted final record, not complete historical records.
					h.errorText = "上次未完整落盘的最后一条额度记录已丢弃，可从源日志重新补回"
					if err := f.Truncate(offset); err != nil {
						h.errorText = "额度历史末行不完整，无法修复: " + err.Error()
					}
				}
			}
			break
		}
		if readErr != nil {
			h.errorText = "额度历史读取中断: " + readErr.Error()
			break
		}
		offset += int64(len(line))
		var record liveQuotaRecord
		if err := json.Unmarshal(line, &record); err != nil || record.ID == "" || record.At.IsZero() {
			h.errorText = "额度历史包含无法解析的记录，已保留原文件；其余记录继续加载"
			continue
		}
		h.records[record.ID] = record
	}
	return h
}

func (h *liveQuotaHistory) existingRecordID(record liveQuotaRecord) string {
	if _, exists := h.records[record.ID]; exists {
		return record.ID
	}
	if record.legacyID != "" {
		if _, exists := h.records[record.legacyID]; exists {
			// The key is opaque: reuse the old saved record rather than adding
			// the same consumption under the corrected conversation identity.
			return record.legacyID
		}
	}
	return record.ID
}

func (h *liveQuotaHistory) put(record liveQuotaRecord) {
	if old, ok := h.records[record.ID]; ok {
		// Later backfill can have a better checkpoint baseline or inherited tier.
		// A quota-only replay must never erase already recorded usage.
		if record.Usage == nil {
			record.Usage = old.Usage
		} else if old.Usage != nil && record.Usage.ServiceTier == "" && old.Usage.ServiceTier != "" {
			record.Usage.ServiceTier = old.Usage.ServiceTier
		}
		if record.Usage != nil && old.Usage != nil && record.Usage.Model == "" {
			record.Usage.Model = old.Usage.Model
		}
		a, _ := json.Marshal(old)
		b, _ := json.Marshal(record)
		if bytes.Equal(a, b) {
			return
		}
	}
	h.records[record.ID] = record
	h.dirty[record.ID] = record
}

func (h *liveQuotaHistory) flush(force bool) {
	if h == nil || len(h.dirty) == 0 || h.path == "" || (!force && time.Since(h.flushedAt) < 2*time.Second) {
		return
	}
	rows := make([]liveQuotaRecord, 0, len(h.dirty))
	for _, record := range h.dirty {
		rows = append(rows, record)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].At.Before(rows[j].At) })
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	for _, record := range rows {
		if err := encoder.Encode(record); err != nil {
			h.errorText = "额度记录编码失败: " + err.Error()
			return
		}
	}
	h.flushedAt = time.Now()
	f, err := os.OpenFile(h.path, os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		h.errorText = "额度历史写入失败，待写内容暂存内存: " + err.Error()
		return
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], info.Size()-1); err != nil {
			h.errorText = "无法检查额度历史末行，待写内容暂存内存: " + err.Error()
			return
		}
		if last[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				h.errorText = "无法恢复额度历史记录边界: " + err.Error()
				return
			}
		}
	}
	if _, err := f.Write(data.Bytes()); err != nil {
		h.errorText = "额度历史写入中断，待写内容暂存内存: " + err.Error()
		return
	}
	h.dirty = map[string]liveQuotaRecord{}
	if strings.HasPrefix(h.errorText, "额度历史写入") {
		h.errorText = ""
	}
}

func (m *liveMonitor) collectQuota(files []*liveFile) {
	sessions := m.sessions()
	for _, file := range files {
		for _, f := range []*liveFile{file, file.backfill} {
			if f == nil || len(f.session.quotaPending) == 0 {
				continue
			}
			if m.quota != nil {
				for _, record := range f.session.quotaPending {
					record.ID = m.quota.existingRecordID(record)
					if record.response != nil {
						fillResponseServiceTier(f.session, record.response, sessions)
						record.Usage = quotaUsageFrom(record.response)
						if record.Usage.ServiceTier == "" && record.response.ServiceTierSource == "" && f.session.ParentID != "" {
							at := record.response.StartedAt
							if turn := f.session.turnsByID[record.response.TurnID]; turn != nil {
								at = turn.StartedAt
							}
							m.quota.waitingTier[record.ID] = liveQuotaTierWait{parent: f.session.ParentID, at: at}
						} else {
							delete(m.quota.waitingTier, record.ID)
						}
					}
					m.quota.put(record)
				}
			}
			f.session.quotaPending = nil
		}
	}
	if m.quota != nil {
		// Parent histories may finish after the child. Retry only unresolved inheritance;
		// never replace an explicitly logged service tier with the parent's current setting.
		for id, pending := range m.quota.waitingTier {
			tier, source := inheritedServiceTier(&liveSession{ParentID: pending.parent}, sessions, pending.at, false)
			if tier == "" || source == "" {
				continue
			}
			record := m.quota.records[id]
			if record.Usage != nil {
				copy := *record.Usage
				copy.ServiceTier = tier
				record.Usage = &copy
				m.quota.put(record)
			}
			delete(m.quota.waitingTier, id)
		}
	}
	m.quota.flush(false)
}

func (h *liveQuotaHistory) description() string {
	if h == nil || h.path == "" {
		return "额度历史仅保存在当前进程内存中"
	}
	return h.path
}
