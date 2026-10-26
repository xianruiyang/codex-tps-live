package codextps

import (
	"context"
	"errors"
	"net/http"
	"time"
)

type liveNodePrepared struct {
	options   liveNodeOptions
	inventory liveNodeInventory
	selected  []*liveNodeSpec
}

func (s *liveNodeService) prepareJob(options liveNodeOptions, requireEngine bool) (*liveNodePrepared, int, error) {
	if len(options.IDs) == 0 || options.Rounds < 1 || options.Rounds > 20 || options.Workers < 1 || options.Workers > 4 || options.Timeout < 3 || options.Timeout > 60 {
		return nil, http.StatusBadRequest, errors.New("请选择节点；轮数 1–20，并发 1–4，超时 3–60 秒")
	}
	inv := discoverLiveNodes(s.home, s.binary)
	if requireEngine && !inv.catalog.Ready {
		return nil, http.StatusBadRequest, errors.New(inv.catalog.Error)
	}
	// Preserve the previous API's two probes when tests is omitted. An explicit
	// empty list means no test was selected, rather than silently running more.
	if options.Tests == nil {
		options.Tests = []string{"http", "websocket"}
	}
	if len(options.Tests) == 0 {
		return nil, http.StatusBadRequest, errors.New("请至少选择一个测速项目")
	}
	testSeen := map[string]bool{}
	for _, test := range options.Tests {
		if (test != "clash" && test != "http" && test != "websocket") || testSeen[test] {
			return nil, http.StatusBadRequest, errors.New("测速项目无效或重复")
		}
		testSeen[test] = true
		if requireEngine && test != "clash" && inv.curl == "" {
			return nil, http.StatusBadRequest, errors.New("Codex HTTP / WebSocket 需要系统 curl.exe")
		}
	}
	if testSeen["clash"] {
		if options.ClashMaxDelayMS == 0 {
			options.ClashMaxDelayMS = 500
		}
		if options.Smart && options.ClashMaxDelayMS < 1 {
			return nil, http.StatusBadRequest, errors.New("智能测速的 Clash 延迟上限必须大于 0 毫秒")
		}
		if options.Timeout > 32 {
			return nil, http.StatusBadRequest, errors.New("Mihomo 普通测速接口的超时上限为 32 秒")
		}
		if options.ClashURL == "" {
			options.ClashURL = inv.catalog.ClashURL
		}
		if !validLiveClashURL(options.ClashURL) {
			return nil, http.StatusBadRequest, errors.New("普通测速地址必须是无用户名和密码的 HTTP / HTTPS URL")
		}
	}
	if options.ClashUnified == nil {
		unified := inv.catalog.UnifiedDelay
		options.ClashUnified = &unified
	}
	var selected []*liveNodeSpec
	seen := map[string]bool{}
	for _, id := range options.IDs {
		spec := inv.specs[id]
		if spec == nil || spec.public.Issue != "" || seen[id] {
			return nil, http.StatusBadRequest, errors.New("节点已变化或无法单独测速，请刷新订阅后重新选择")
		}
		seen[id] = true
		selected = append(selected, spec)
	}
	return &liveNodePrepared{options, inv, selected}, 0, nil
}
func cloneLiveNodeJob(job *liveNodeJob) *liveNodeJob {
	if job == nil {
		return nil
	}
	copy := *job
	copy.Nodes = append([]liveNode{}, job.Nodes...)
	copy.Results = append([]liveNodeResult{}, job.Results...)
	copy.Exclusions = append([]liveNodeExclusion{}, job.Exclusions...)
	return &copy
}

type liveNodePlanRun struct {
	Revision, Interval int
	Auto               bool
}

func (s *liveNodeService) startJob(options liveNodeOptions, planID string, completed func(*liveNodeJob), planState ...liveNodePlanRun) (*liveNodeJob, int, error) {
	prepared, code, err := s.prepareJob(options, true)
	if err != nil {
		return nil, code, err
	}
	options, inv, selected := prepared.options, prepared.inventory, prepared.selected
	s.mu.Lock()
	if (s.job != nil && liveNodeBusy(s.job.Status)) || s.inFlightLocked() {
		s.mu.Unlock()
		return nil, http.StatusConflict, errors.New("已有测速任务，请先停止或等待完成")
	}
	job := &liveNodeJob{ID: liveNodeID(time.Now().String()), Status: "preparing", Message: "正在准备独立测速内核",
		PlanID: planID, Started: time.Now(), Options: options, Nodes: []liveNode{}, Results: []liveNodeResult{}, Exclusions: []liveNodeExclusion{},
		Total: len(selected) * options.Rounds * len(options.Tests), Subscriptions: map[string]string{}}
	if len(planState) > 0 {
		job.PlanRevision, job.PlanInterval, job.PlanAuto = planState[0].Revision, planState[0].Interval, planState[0].Auto
	}
	job.NodeKeys = map[string]string{}
	for _, node := range selected {
		job.Nodes = append(job.Nodes, node.public)
		job.NodeKeys[node.public.ID] = node.public.ID
	}
	for _, sub := range inv.catalog.Subscriptions {
		job.Subscriptions[sub.ID] = sub.Name
	}
	s.launchJobLocked(prepared, job, completed)
	copy := cloneLiveNodeJob(job)
	s.mu.Unlock()
	return copy, 0, nil
}
func (s *liveNodeService) launchJobLocked(prepared *liveNodePrepared, job *liveNodeJob, completed func(*liveNodeJob)) {
	ctx, cancel := context.WithCancel(s.ctx)
	s.job, s.cancel, s.finished = job, cancel, make(chan struct{})
	done := s.finished
	s.saveJobLocked(job)
	go func() {
		defer close(done)
		defer cancel()
		s.run(ctx, prepared.inventory, prepared.selected, job)
		if completed != nil {
			s.mu.Lock()
			copy := cloneLiveNodeJob(job)
			s.mu.Unlock()
			completed(copy)
		}
	}()
}
