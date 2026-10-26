package codextps

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type nodePlanRef struct {
	Key              string `json:"key"`
	Subscription     string `json:"subscription"`
	SubscriptionName string `json:"subscription_name"`
	Name             string `json:"name"`
}
type nodePlan struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Enabled   bool            `json:"enabled"`
	Cancelled bool            `json:"cancelled,omitempty"`
	Interval  int             `json:"interval_seconds"`
	Revision  int             `json:"revision"`
	Nodes     []nodePlanRef   `json:"nodes"`
	Options   liveNodeOptions `json:"options"`
	Created   time.Time       `json:"created"`
	Updated   time.Time       `json:"updated"`
	Next      *time.Time      `json:"next_run,omitempty"`
	Last      *time.Time      `json:"last_run,omitempty"`
	Running   bool            `json:"running"`
	Message   string          `json:"message"`
}
type nodePlanPoint struct {
	Run      string      `json:"run"`
	Node     nodePlanRef `json:"node"`
	Test     string      `json:"test"`
	At       time.Time   `json:"at"`
	Revision int         `json:"revision"`
	Success  int         `json:"success"`
	Failure  int         `json:"failure"`
	Skipped  int         `json:"skipped"`
	Values   []float64   `json:"values,omitempty"`
	Outcome  string      `json:"outcome"`
	Note     string      `json:"note,omitempty"`
	Median   *float64    `json:"median_ms"`
	Interval int         `json:"interval_seconds"`
}
type nodePlanStats struct {
	Node      nodePlanRef `json:"node"`
	Success   int         `json:"success"`
	Failure   int         `json:"failure"`
	Skipped   int         `json:"skipped"`
	Samples   int         `json:"samples"`
	Rate      *float64    `json:"success_rate"`
	Coverage  *float64    `json:"coverage"`
	Median    *float64    `json:"median_ms"`
	P95       *float64    `json:"p95_ms"`
	Deviation *float64    `json:"deviation_ms"`
	Streak    int         `json:"failure_streak"`
	Longest   int         `json:"longest_failure_streak"`
	Latest    string      `json:"latest"`
	LatestAt  *time.Time  `json:"latest_at,omitempty"`
}
type nodePlanManager struct {
	svc            *liveNodeService
	db             *sql.DB
	mu             sync.Mutex
	plans          map[string]*nodePlan
	active         string
	owner          bool
	errorText      string
	release        func()
	stopped        chan struct{}
	retryRecovery  time.Time
	recoveryWrites sync.WaitGroup
}

func openNodePlans(svc *liveNodeService, dataDir string) *nodePlanManager {
	m := &nodePlanManager{svc: svc, plans: map[string]*nodePlan{}, stopped: make(chan struct{})}
	if dataDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			m.errorText = "无法定位计划数据目录"
			close(m.stopped)
			return m
		}
		dataDir = filepath.Join(base, "CodexTPSLive")
	}
	dir, err := filepath.Abs(filepath.Join(dataDir, "node-plans"))
	if err != nil || os.MkdirAll(dir, 0700) != nil {
		m.errorText = "无法创建计划数据目录"
		close(m.stopped)
		return m
	}
	m.release, err = lockLiveNodePlans(filepath.Join(dir, "scheduler.lock"))
	m.owner = err == nil
	db, err := sql.Open("sqlite", filepath.Join(dir, "history.sqlite"))
	if err != nil {
		m.errorText = "无法打开计划数据库"
		m.closeUnused()
		return m
	}
	m.db = db
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS plans(id TEXT PRIMARY KEY,data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS node_current_job(slot INTEGER PRIMARY KEY,data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS node_job_results(run TEXT NOT NULL,node TEXT NOT NULL,test TEXT NOT NULL,round INTEGER NOT NULL,data TEXT NOT NULL,PRIMARY KEY(run,node,test,round));
CREATE TABLE IF NOT EXISTS runs(id TEXT PRIMARY KEY,plan TEXT NOT NULL,revision INTEGER NOT NULL,at INTEGER NOT NULL,data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS points(run TEXT NOT NULL,plan TEXT NOT NULL,revision INTEGER NOT NULL,node TEXT NOT NULL,test TEXT NOT NULL,at INTEGER NOT NULL,data TEXT NOT NULL,PRIMARY KEY(run,node,test));
CREATE INDEX IF NOT EXISTS points_window ON points(plan,test,revision,at);
CREATE INDEX IF NOT EXISTS runs_plan ON runs(plan,at);`)
	if err != nil {
		m.errorText = "无法初始化计划数据库"
		m.closeUnused()
		return m
	}
	rows, err := db.Query("SELECT data FROM plans")
	if err != nil {
		m.errorText = "无法读取计划"
		m.closeUnused()
		return m
	}
	for rows.Next() {
		var raw string
		var p nodePlan
		if rows.Scan(&raw) != nil || json.Unmarshal([]byte(raw), &p) != nil {
			m.errorText = "计划记录无法解析，已保留原文件"
			continue
		}
		if p.Running {
			p.Message = "上一轮因服务退出中断；未完成批次可能没有历史记录"
		}
		p.Running = false
		if p.Cancelled {
			p.Enabled = false
			p.Next = nil
			p.Message = "计划已取消，历史已保留"
		}
		if p.Enabled {
			if p.Next == nil {
				at := time.Now().Add(time.Duration(p.Interval) * time.Second)
				p.Next = &at
			}
			p.Message = "服务已恢复，按原计划继续；中断批次优先续跑"
		}
		m.plans[p.ID] = &p
	}
	if rows.Err() != nil {
		m.errorText = "计划读取中断，已保留数据库"
	}
	rows.Close()
	if m.errorText != "" {
		m.owner = false
	}
	if m.owner {
		if err := svc.loadJob(db); err != nil {
			m.errorText = "测速进度读取失败，已保留数据库：" + err.Error()
			m.owner = false
			svc.db = nil
		}
	}
	go m.loop()
	return m
}
func (m *nodePlanManager) closeUnused() {
	if m.db != nil {
		m.db.Close()
		m.db = nil
	}
	if m.release != nil {
		m.release()
		m.release = nil
	}
	close(m.stopped)
}
func (m *nodePlanManager) close() {
	<-m.stopped
	m.recoveryWrites.Wait()
	if m.db != nil {
		m.db.Close()
	}
	if m.release != nil {
		m.release()
	}
}
func (m *nodePlanManager) persist(p *nodePlan) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = m.db.Exec("INSERT INTO plans(id,data) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", p.ID, string(raw))
	return err
}

func (m *nodePlanManager) saveState(p *nodePlan) {
	if err := m.persist(p); err != nil {
		m.errorText = "计划状态保存失败，请检查本机数据目录"
	}
}
func (m *nodePlanManager) list() any {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := []nodePlan{}
	for _, p := range m.plans {
		all = append(all, *p)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Created.Before(all[j].Created) })
	return map[string]any{"plans": all, "owner": m.owner, "error": m.errorText}
}
func (m *nodePlanManager) loop() {
	defer close(m.stopped)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.svc.ctx.Done():
			return
		case <-ticker.C:
			m.tick()
		}
	}
}
func (m *nodePlanManager) tick() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.owner || m.db == nil || m.svc.ctx.Err() != nil || m.active != "" {
		return
	}
	if m.recoverJobLocked() {
		return
	}
	var due *nodePlan
	now := time.Now()
	for _, p := range m.plans {
		if p.Enabled && !p.Cancelled && p.Next != nil && !p.Next.After(now) && (due == nil || p.Next.Before(*due.Next)) {
			due = p
		}
	}
	if due != nil {
		m.startLocked(due)
	}
}
func (m *nodePlanManager) resolve(p *nodePlan) ([]string, []string) {
	inv := discoverLiveNodes(m.svc.home, m.svc.binary)
	ids, missing := []string{}, []string{}
	for _, ref := range p.Nodes {
		match := ""
		for id, spec := range inv.specs {
			if spec.public.Subscription == ref.Subscription && spec.public.Name == ref.Name && spec.public.Issue == "" {
				if match != "" {
					match = ""
					break
				}
				match = id
			}
		}
		if match == "" {
			missing = append(missing, ref.Name)
		} else {
			ids = append(ids, match)
		}
	}
	return ids, missing
}
func (m *nodePlanManager) startLocked(p *nodePlan) error {
	if p.Cancelled {
		return errors.New("请先恢复已取消的计划")
	}
	if m.active != "" {
		return errors.New("已有计划执行中")
	}
	m.svc.mu.Lock()
	busy := m.svc.job != nil && liveNodeBusy(m.svc.job.Status)
	m.svc.mu.Unlock()
	if busy {
		p.Message = "等待当前测速结束"
		return errors.New(p.Message)
	}
	ids, missing := m.resolve(p)
	if len(missing) > 0 {
		p.Message = fmt.Sprintf("%d 个节点已变化，请编辑计划重新选择", len(missing))
		at := time.Now().Add(time.Duration(p.Interval) * time.Second)
		p.Next = &at
		m.saveState(p)
		return errors.New(p.Message)
	}
	options := p.Options
	options.IDs = ids
	snapshot := *p
	job, code, err := m.svc.startJob(options, p.ID, func(job *liveNodeJob) { m.complete(snapshot, job) }, liveNodePlanRun{snapshot.Revision, snapshot.Interval, snapshot.Enabled})
	if err != nil {
		if code == http.StatusConflict {
			p.Message = "等待当前测速结束"
			return errors.New(p.Message)
		}
		p.Message = err.Error()
		at := time.Now().Add(time.Duration(p.Interval) * time.Second)
		p.Next = &at
		m.saveState(p)
		return err
	}
	m.active = p.ID
	p.Running = true
	p.Last = &job.Started
	p.Next = nil
	p.Message = "正在执行本轮计划"
	m.saveState(p)
	return nil
}
func (m *nodePlanManager) complete(snapshot nodePlan, job *liveNodeJob) {
	if job.Status == "interrupted" {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.active = ""
		if p := m.plans[snapshot.ID]; p != nil {
			p.Running = false
			p.Next = nil
			p.Message = "已保存测速进度，服务启动后继续未完成项"
			m.saveState(p)
		}
		return
	}
	refs := map[string]nodePlanRef{}
	for _, node := range job.Nodes {
		refs[node.ID] = nodePlanRef{Key: liveNodeID(node.Subscription, node.Name), Subscription: node.Subscription, SubscriptionName: job.Subscriptions[node.Subscription], Name: node.Name}
	}
	points := map[string]*nodePlanPoint{}
	for _, row := range job.Results {
		ref := refs[row.Node]
		key := ref.Key + "/" + row.Endpoint
		point := points[key]
		if point == nil {
			point = &nodePlanPoint{Run: job.ID, Node: ref, Test: row.Endpoint, At: job.Started, Revision: snapshot.Revision, Values: []float64{}, Interval: snapshot.Interval}
			points[key] = point
		}
		point.Outcome = row.Outcome
		if row.Note != "" {
			point.Note = row.Note
		}
		if row.Outcome == "skipped" {
			point.Skipped++
		} else if row.Outcome == "clash_ok" || row.Outcome == "auth_reached" || row.Outcome == "upgrade_accepted" {
			point.Success++
			value := row.TTFB
			if row.Endpoint == "clash" {
				value = row.Delay
			}
			if value != nil {
				point.Values = append(point.Values, *value)
			}
		} else {
			point.Failure++
		}
	}
	for _, point := range points {
		sort.Float64s(point.Values)
		point.Median = planQuantile(point.Values, .5)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	defer func() { m.active = "" }()
	tx, err := m.db.Begin()
	if err == nil {
		summary, _ := json.Marshal(map[string]any{"status": job.Status, "message": job.Message, "done": job.Done, "total": job.Total, "skipped": job.Skipped, "started": job.Started, "finished": job.Finished})
		_, err = tx.Exec("INSERT OR IGNORE INTO runs(id,plan,revision,at,data) VALUES(?,?,?,?,?)", job.ID, snapshot.ID, snapshot.Revision, job.Started.UnixMilli(), string(summary))
		if err == nil {
			for _, point := range points {
				raw, _ := json.Marshal(point)
				_, err = tx.Exec("INSERT OR IGNORE INTO points(run,plan,revision,node,test,at,data) VALUES(?,?,?,?,?,?,?)", job.ID, snapshot.ID, snapshot.Revision, point.Node.Key, point.Test, point.At.UnixMilli(), string(raw))
				if err != nil {
					break
				}
			}
		}
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
	}
	p := m.plans[snapshot.ID]
	if p == nil {
		return
	}
	p.Running = false
	p.Message = job.Message
	if p.Cancelled {
		p.Message = "计划已取消，历史已保留"
	}
	if err != nil {
		p.Message = "历史记录写入失败，请检查本机数据目录"
	}
	if p.Enabled {
		at := time.Now().Add(time.Duration(p.Interval) * time.Second)
		p.Next = &at
	}
	if saveErr := m.persist(p); saveErr != nil {
		m.errorText = "计划状态保存失败"
	}
	if err == nil {
		m.svc.markRecorded(job.ID)
	}
}
func planQuantile(values []float64, q float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	index := q * float64(len(values)-1)
	a := int(index)
	b := min(a+1, len(values)-1)
	v := values[a] + (values[b]-values[a])*(index-float64(a))
	return &v
}

func (m *nodePlanManager) register(mux *http.ServeMux) {
	mux.HandleFunc("/node-plans", func(w http.ResponseWriter, r *http.Request) {
		page, _ := liveAssets.ReadFile("web/node_plans.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	mux.HandleFunc("/api/node-plans", func(w http.ResponseWriter, r *http.Request) { writeLiveJSON(w, m.list()) })
	mux.HandleFunc("/api/node-plans/save", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		var request struct {
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Enabled  bool            `json:"enabled"`
			Interval int             `json:"interval_seconds"`
			Options  liveNodeOptions `json:"options"`
		}
		decodeErr := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request)
		request.Name = strings.TrimSpace(request.Name)
		if decodeErr != nil || request.Name == "" || utf8.RuneCountInString(request.Name) > 80 || request.Interval < 1 || request.Interval > 31536000 {
			http.Error(w, "请输入计划名称与正数间隔（秒）", 400)
			return
		}
		prepared, code, err := m.svc.prepareJob(request.Options, false)
		if err != nil {
			http.Error(w, err.Error(), code)
			return
		}
		subNames := map[string]string{}
		for _, sub := range prepared.inventory.catalog.Subscriptions {
			subNames[sub.ID] = sub.Name
		}
		refs := []nodePlanRef{}
		for _, spec := range prepared.selected {
			node := spec.public
			matches := 0
			for _, candidate := range prepared.inventory.specs {
				if candidate.public.Subscription == node.Subscription && candidate.public.Name == node.Name {
					matches++
				}
			}
			if matches != 1 {
				http.Error(w, "同一订阅内存在同名节点，无法稳定匹配计划："+node.Name, 400)
				return
			}
			refs = append(refs, nodePlanRef{Key: liveNodeID(node.Subscription, node.Name), Subscription: node.Subscription, SubscriptionName: subNames[node.Subscription], Name: node.Name})
		}
		sort.Slice(refs, func(i, j int) bool { return refs[i].Key < refs[j].Key })
		m.mu.Lock()
		defer m.mu.Unlock()
		if !m.owner || m.db == nil {
			http.Error(w, "计划数据库不可写，或由另一监控实例管理", 409)
			return
		}
		now := time.Now()
		p := nodePlan{ID: request.ID, Name: request.Name, Enabled: request.Enabled, Interval: request.Interval, Nodes: refs, Options: prepared.options, Created: now, Updated: now, Revision: 1, Message: "已保存，等待启动"}
		p.Options.IDs = nil
		if request.ID != "" {
			old := m.plans[request.ID]
			if old == nil {
				http.Error(w, "计划不存在", 404)
				return
			}
			if old.Running {
				http.Error(w, "请先暂停正在运行的计划再编辑", 409)
				return
			}
			if old.Cancelled {
				http.Error(w, "请先恢复已取消的计划再编辑", 409)
				return
			}
			p.Created = old.Created
			p.Revision = old.Revision
			before, _ := json.Marshal(struct {
				Nodes    []nodePlanRef
				Options  liveNodeOptions
				Interval int
			}{old.Nodes, old.Options, old.Interval})
			after, _ := json.Marshal(struct {
				Nodes    []nodePlanRef
				Options  liveNodeOptions
				Interval int
			}{p.Nodes, p.Options, p.Interval})
			if string(before) != string(after) {
				p.Revision++
			}
			p.Last = old.Last
		} else {
			p.ID = liveNodeID(now.String(), p.Name)
		}
		if p.Enabled {
			p.Next = &now
			p.Message = "等待执行首轮"
		} else {
			p.Message = "已保存，计划处于暂停状态"
		}
		if err = m.persist(&p); err != nil {
			http.Error(w, "计划保存失败", 500)
			return
		}
		m.plans[p.ID] = &p
		writeLiveJSON(w, p)
	})
	mux.HandleFunc("/api/node-plans/action", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		var request struct {
			ID     string `json:"id"`
			Action string `json:"action"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&request) != nil {
			http.Error(w, "无效操作", 400)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if !m.owner || m.db == nil {
			http.Error(w, "计划由另一监控实例管理或数据不可写", 409)
			return
		}
		p := m.plans[request.ID]
		if p == nil {
			http.Error(w, "计划不存在", 404)
			return
		}
		now := time.Now()
		switch request.Action {
		case "pause", "cancel":
			p.Enabled = false
			p.Next = nil
			p.Message = "计划已暂停"
			if request.Action == "cancel" {
				p.Cancelled = true
				p.Message = "计划已取消，正在停止当前测试；历史保留"
			}
			m.svc.mu.Lock()
			if m.svc.job != nil && m.svc.job.PlanID == p.ID && liveNodeBusy(m.svc.job.Status) {
				m.svc.stopJobLocked()
			}
			m.svc.mu.Unlock()
			if p.Cancelled && !p.Running {
				p.Message = "计划已取消，历史已保留"
			}
		case "restore":
			p.Cancelled = false
			p.Enabled = false
			p.Next = nil
			p.Message = "计划已恢复，处于暂停状态"
		case "resume":
			if p.Cancelled {
				http.Error(w, "请先恢复已取消的计划", 409)
				return
			}
			p.Enabled = true
			p.Next = &now
			p.Message = "等待执行"
		case "run":
			if m.active != "" {
				http.Error(w, "已有计划执行中", 409)
				return
			}
			if err := m.startLocked(p); err != nil {
				http.Error(w, err.Error(), 409)
				return
			}
		default:
			http.Error(w, "未知操作", 400)
			return
		}
		p.Updated = now
		if err := m.persist(p); err != nil {
			http.Error(w, "计划状态保存失败", 500)
			return
		}
		writeLiveJSON(w, p)
	})
	mux.HandleFunc("/api/node-plans/selection", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		p := m.plans[r.URL.Query().Get("id")]
		if p == nil {
			m.mu.Unlock()
			http.Error(w, "计划不存在", 404)
			return
		}
		copy := *p
		m.mu.Unlock()
		ids, missing := m.resolve(&copy)
		writeLiveJSON(w, map[string]any{"plan": copy, "ids": ids, "missing": missing})
	})
	mux.HandleFunc("/api/node-plans/history", m.history)
	mux.HandleFunc("/api/node-plans/export", m.export)
}

func (m *nodePlanManager) readHistory(ctx context.Context, id, test string, revision int, from, to time.Time) ([]nodePlanPoint, error) {
	if m.db == nil {
		return nil, errors.New("计划历史不可用")
	}
	query := "SELECT data FROM points WHERE plan=? AND test=? AND at>=? AND at<=?"
	args := []any{id, test, from.UnixMilli(), to.UnixMilli()}
	if revision > 0 {
		query += " AND revision=?"
		args = append(args, revision)
	}
	query += " ORDER BY at,run,node"
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := []nodePlanPoint{}
	for rows.Next() {
		var raw string
		var point nodePlanPoint
		if rows.Scan(&raw) != nil || json.Unmarshal([]byte(raw), &point) != nil {
			return nil, errors.New("历史点无法解析")
		}
		points = append(points, point)
	}
	return points, rows.Err()
}
func (m *nodePlanManager) historyParameters(r *http.Request) (*nodePlan, string, int, time.Time, time.Time, error) {
	m.mu.Lock()
	p := m.plans[r.URL.Query().Get("id")]
	var copy nodePlan
	if p != nil {
		copy = *p
	}
	m.mu.Unlock()
	if p == nil {
		return nil, "", 0, time.Time{}, time.Time{}, errors.New("计划不存在")
	}
	test := r.URL.Query().Get("test")
	if test == "" {
		test = copy.Options.Tests[0]
	}
	if test != "clash" && test != "http" && test != "websocket" {
		return nil, "", 0, time.Time{}, time.Time{}, errors.New("测速项目无效")
	}
	to := time.Now()
	from := to.Add(-24 * time.Hour)
	if text := r.URL.Query().Get("from"); text != "" {
		parsed, err := time.Parse(time.RFC3339, text)
		if err != nil {
			return nil, "", 0, from, to, errors.New("开始时间无效")
		}
		from = parsed
	}
	if text := r.URL.Query().Get("to"); text != "" {
		parsed, err := time.Parse(time.RFC3339, text)
		if err != nil {
			return nil, "", 0, from, to, errors.New("结束时间无效")
		}
		to = parsed
	}
	if !from.Before(to) {
		return nil, "", 0, from, to, errors.New("结束时间必须晚于开始时间")
	}
	revision := copy.Revision
	if r.URL.Query().Get("revision") == "all" {
		revision = 0
	}
	return &copy, test, revision, from, to, nil
}
func (m *nodePlanManager) history(w http.ResponseWriter, r *http.Request) {
	p, test, revision, from, to, err := m.historyParameters(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	points, err := m.readHistory(r.Context(), p.ID, test, revision, from, to)
	if err != nil {
		http.Error(w, "无法读取计划历史", 500)
		return
	}
	stats := map[string]*nodePlanStats{}
	values := map[string][]float64{}
	series := map[string][]nodePlanPoint{}
	for _, ref := range p.Nodes {
		stats[ref.Key] = &nodePlanStats{Node: ref}
	}
	for _, point := range points {
		s := stats[point.Node.Key]
		if s == nil {
			s = &nodePlanStats{Node: point.Node}
			stats[point.Node.Key] = s
		}
		s.Success += point.Success
		s.Failure += point.Failure
		s.Skipped += point.Skipped
		s.Samples += point.Success + point.Failure
		s.Latest = point.Outcome
		at := point.At
		s.LatestAt = &at
		if point.Success > 0 {
			s.Streak = 0
		} else if point.Failure > 0 {
			s.Streak++
			s.Longest = max(s.Longest, s.Streak)
		}
		values[point.Node.Key] = append(values[point.Node.Key], point.Values...)
		series[point.Node.Key] = append(series[point.Node.Key], point)
	}
	all := []nodePlanStats{}
	for key, s := range stats {
		v := values[key]
		sort.Float64s(v)
		s.Median = planQuantile(v, .5)
		s.P95 = planQuantile(v, .95)
		if s.Samples > 0 {
			rate := float64(s.Success) / float64(s.Samples) * 100
			s.Rate = &rate
		}
		if total := s.Samples + s.Skipped; total > 0 {
			coverage := float64(s.Samples) / float64(total) * 100
			s.Coverage = &coverage
		}
		if len(v) > 0 {
			mean := 0.0
			for _, n := range v {
				mean += n
			}
			mean /= float64(len(v))
			variance := 0.0
			for _, n := range v {
				variance += (n - mean) * (n - mean)
			}
			deviation := math.Sqrt(variance / float64(len(v)))
			s.Deviation = &deviation
		}
		all = append(all, *s)
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.Rate == nil && b.Rate == nil {
			return a.Node.Key < b.Node.Key
		}
		if a.Rate == nil {
			return false
		}
		if b.Rate == nil {
			return true
		}
		if *a.Rate != *b.Rate {
			return *a.Rate > *b.Rate
		}
		if a.Samples != b.Samples {
			return a.Samples > b.Samples
		}
		if a.P95 != nil && b.P95 != nil && *a.P95 != *b.P95 {
			return *a.P95 < *b.P95
		}
		if a.Node.Name == b.Node.Name {
			return a.Node.Key < b.Node.Key
		}
		return a.Node.Name < b.Node.Name
	})
	for key, s := range series {
		if len(s) <= 600 {
			continue
		}
		buckets := map[int64]*nodePlanPoint{}
		order := []int64{}
		width := max(int64(1), to.Sub(from).Milliseconds()/600)
		for _, point := range s {
			bucket := (point.At.UnixMilli() - from.UnixMilli()) / width
			aggregate := buckets[bucket]
			if aggregate == nil {
				copy := point
				copy.Values = append([]float64{}, point.Values...)
				buckets[bucket] = &copy
				order = append(order, bucket)
			} else {
				aggregate.At = point.At
				aggregate.Success += point.Success
				aggregate.Failure += point.Failure
				aggregate.Skipped += point.Skipped
				aggregate.Values = append(aggregate.Values, point.Values...)
			}
		}
		compressed := []nodePlanPoint{}
		for _, bucket := range order {
			point := buckets[bucket]
			sort.Float64s(point.Values)
			point.Median = planQuantile(point.Values, .5)
			point.Values = nil
			compressed = append(compressed, *point)
		}
		series[key] = compressed
	}
	writeLiveJSON(w, map[string]any{"plan": p, "test": test, "from": from, "to": to, "revision": revision, "stats": all, "series": series, "point_count": len(points)})
}
func (m *nodePlanManager) export(w http.ResponseWriter, r *http.Request) {
	p, test, revision, from, to, err := m.historyParameters(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	points, err := m.readHistory(r.Context(), p.ID, test, revision, from, to)
	if err != nil {
		http.Error(w, "历史导出失败", 500)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="node-plan-history.csv"`)
	w.Write([]byte{0xef, 0xbb, 0xbf})
	writer := csv.NewWriter(w)
	writer.Write([]string{"计划", "配置版本", "时间", "订阅", "节点", "项目", "成功", "失败", "跳过", "中位数毫秒", "最近结果", "判定原因"})
	for _, point := range points {
		latency := ""
		if point.Median != nil {
			latency = strconv.FormatFloat(*point.Median, 'f', 2, 64)
		}
		writer.Write([]string{safeLiveCSV(p.Name), strconv.Itoa(point.Revision), point.At.Format(time.RFC3339), safeLiveCSV(point.Node.SubscriptionName), safeLiveCSV(point.Node.Name), test, strconv.Itoa(point.Success), strconv.Itoa(point.Failure), strconv.Itoa(point.Skipped), latency, point.Outcome, safeLiveCSV(point.Note)})
	}
	writer.Flush()
}
