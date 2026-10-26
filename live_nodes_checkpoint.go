package codextps

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Only public node identities and probe results are persisted, never proxy credentials.
func (s *liveNodeService) saveJobLocked(job *liveNodeJob) error {
	if s.db == nil {
		job.PersistenceError = "进度存储不可用，本次无法保证重启续跑"
		return nil
	}
	copy := *job
	copy.Results, copy.Exclusions = nil, nil
	raw, err := json.Marshal(copy)
	if err == nil {
		_, err = s.db.Exec("INSERT INTO node_current_job(slot,data) VALUES(1,?) ON CONFLICT(slot) DO UPDATE SET data=excluded.data", string(raw))
	}
	if err != nil {
		job.PersistenceError = "测速进度保存失败，请检查数据目录"
	}
	return err
}

func (s *liveNodeService) saveResultLocked(job *liveNodeJob, row liveNodeResult) {
	if s.db == nil {
		job.PersistenceError = "进度存储不可用，本次无法保证重启续跑"
		return
	}
	stored := row
	stored.Node = job.NodeKeys[row.Node]
	if stored.Node == "" {
		stored.Node = row.Node
	}
	raw, err := json.Marshal(stored)
	if err == nil {
		_, err = s.db.Exec("INSERT OR IGNORE INTO node_job_results(run,node,test,round,data) VALUES(?,?,?,?,?)", job.ID, stored.Node, row.Endpoint, row.Round, string(raw))
	}
	if err != nil {
		job.PersistenceError = "部分测速结果保存失败，重启可能需要重测该项"
	}
}

func (s *liveNodeService) loadJob(db *sql.DB) error {
	s.db = db
	var raw string
	err := db.QueryRow("SELECT data FROM node_current_job WHERE slot=1").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var job liveNodeJob
	if err = json.Unmarshal([]byte(raw), &job); err != nil {
		return err
	}
	if job.NodeKeys == nil {
		job.NodeKeys = map[string]string{}
		for _, node := range job.Nodes {
			job.NodeKeys[node.ID] = node.ID
		}
	}
	runtimeIDs := map[string]string{}
	for id, key := range job.NodeKeys {
		runtimeIDs[key] = id
	}
	rows, err := db.Query("SELECT data FROM node_job_results WHERE run=? ORDER BY rowid", job.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	job.Results, job.Exclusions = []liveNodeResult{}, []liveNodeExclusion{}
	job.Skipped = 0
	for rows.Next() {
		var row liveNodeResult
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(raw), &row); err != nil {
			return err
		}
		id := runtimeIDs[row.Node]
		if id == "" {
			return errors.New("测速进度中的节点身份无法解析")
		}
		row.Node = id
		job.Results = append(job.Results, row)
		if row.Outcome == "skipped" {
			job.Skipped++
		}
		if job.Options.Smart {
			if block := liveNodeSmartExclusion(job.Options, row); block != nil {
				job.Exclusions = append(job.Exclusions, *block)
			}
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	job.Done = len(job.Results)
	if liveNodeBusy(job.Status) || job.Status == "interrupted" {
		if job.StopRequested {
			job.Status, job.Message = "cancelled", "上次测速已主动停止"
		} else {
			job.Status, job.Message = "interrupted", "已读取上次进度，等待继续未完成项"
			job.Finished = nil
		}
	}
	s.job = &job
	return nil
}

func liveNodeResultKey(node, endpoint string, round int) string {
	return fmt.Sprintf("%s/%s/%d", node, endpoint, round)
}
func liveNodeCompleted(job *liveNodeJob) map[string]bool {
	done := map[string]bool{}
	for _, row := range job.Results {
		done[liveNodeResultKey(row.Node, row.Endpoint, row.Round)] = true
	}
	return done
}
func (s *liveNodeService) inFlightLocked() bool {
	if s.finished == nil {
		return false
	}
	select {
	case <-s.finished:
		return false
	default:
		return true
	}
}
func (s *liveNodeService) stopJobLocked() {
	if s.job == nil {
		return
	}
	s.job.StopRequested = true
	if s.inFlightLocked() {
		s.job.Status, s.job.Message = "stopping", "正在停止请求并清理临时内核"
		if s.cancel != nil {
			s.cancel()
		}
	} else {
		now := time.Now()
		s.job.Status, s.job.Message, s.job.Finished = "cancelled", "测速已主动停止，已保留完成项", &now
	}
	s.saveJobLocked(s.job)
}
func (s *liveNodeService) markRecorded(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != nil && s.job.ID == id {
		s.job.Recorded = true
		s.saveJobLocked(s.job)
	}
}

func (s *liveNodeService) resumeJob(completed func(*liveNodeJob)) (*liveNodeJob, error) {
	s.mu.Lock()
	job := cloneLiveNodeJob(s.job)
	s.mu.Unlock()
	if job == nil || job.Status != "interrupted" {
		return nil, errors.New("没有待续跑的测速")
	}
	inv := discoverLiveNodes(s.home, s.binary)
	ids, remap, keys := []string{}, map[string]string{}, map[string]string{}
	for _, node := range job.Nodes {
		match := ""
		for id, spec := range inv.specs {
			if spec.public.Subscription == node.Subscription && spec.public.Name == node.Name && spec.public.Issue == "" {
				if match != "" {
					return nil, errors.New("续跑节点存在同名歧义：" + node.Name)
				}
				match = id
			}
		}
		if match == "" {
			return nil, errors.New("续跑节点暂不可用：" + node.Name)
		}
		ids = append(ids, match)
		remap[node.ID] = match
		keys[match] = job.NodeKeys[node.ID]
	}
	options := job.Options
	options.IDs = ids
	prepared, _, err := s.prepareJob(options, true)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil || s.job.ID != job.ID || s.job.Status != "interrupted" || s.inFlightLocked() {
		return nil, errors.New("续跑状态已变化")
	}
	job = s.job
	for i := range job.Results {
		job.Results[i].Node = remap[job.Results[i].Node]
	}
	for i := range job.Exclusions {
		job.Exclusions[i].Node = remap[job.Exclusions[i].Node]
	}
	job.Nodes = []liveNode{}
	for _, spec := range prepared.selected {
		job.Nodes = append(job.Nodes, spec.public)
	}
	job.Options, job.NodeKeys, job.Finished, job.Cleaned = prepared.options, keys, nil, false
	job.Status, job.Message = "preparing", fmt.Sprintf("继续上次测速：已保留 %d/%d 项", job.Done, job.Total)
	s.launchJobLocked(prepared, job, completed)
	return cloneLiveNodeJob(job), nil
}

func nodePlanRunSnapshot(job *liveNodeJob) nodePlan {
	return nodePlan{ID: job.PlanID, Revision: job.PlanRevision, Interval: job.PlanInterval, Enabled: job.PlanAuto}
}

// Called under the manager lock. Recovery takes precedence over another plan.
func (m *nodePlanManager) recoverJobLocked() bool {
	s := m.svc
	s.mu.Lock()
	job := cloneLiveNodeJob(s.job)
	flight := s.inFlightLocked()
	s.mu.Unlock()
	if job == nil || flight {
		return false
	}
	if job.PlanID != "" && job.Status != "interrupted" && !job.Recorded {
		if m.plans[job.PlanID] == nil {
			s.markRecorded(job.ID)
			return false
		}
		// complete takes m.mu; run it outside this tick's lock, serialized by active.
		m.active = job.PlanID
		m.recoveryWrites.Add(1)
		go func() { defer m.recoveryWrites.Done(); m.complete(nodePlanRunSnapshot(job), job) }()
		return true
	}
	if job.Status != "interrupted" {
		return false
	}
	p := m.plans[job.PlanID]
	if job.PlanID != "" && (p == nil || p.Cancelled || p.Revision != job.PlanRevision || (job.PlanAuto && !p.Enabled)) {
		s.mu.Lock()
		s.stopJobLocked()
		s.mu.Unlock()
		return true
	}
	if time.Now().Before(m.retryRecovery) {
		return true
	}
	var callback func(*liveNodeJob)
	if p != nil {
		callback = func(result *liveNodeJob) { m.complete(nodePlanRunSnapshot(result), result) }
	}
	resumed, err := s.resumeJob(callback)
	if err != nil {
		m.retryRecovery = time.Now().Add(15 * time.Second)
		s.mu.Lock()
		if s.job != nil {
			s.job.Message = "等待续跑：" + err.Error() + "；15 秒后重试，可主动停止"
		}
		s.mu.Unlock()
		if p != nil {
			p.Message = "等待续跑：" + err.Error()
			m.saveState(p)
		}
		return true
	}
	if p != nil {
		m.active = p.ID
		p.Running = true
		p.Next = nil
		p.Last = &resumed.Started
		p.Message = resumed.Message
		m.saveState(p)
	}
	return true
}
