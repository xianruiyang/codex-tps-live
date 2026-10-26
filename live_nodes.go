package codextps

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"
)

var liveNodeEndpoints = map[string]string{
	"http":      "https://chatgpt.com/backend-api/codex/models?client_version=0.164.0-alpha.11.1",
	"websocket": "https://chatgpt.com/backend-api/codex/responses",
}

type liveNodeOptions struct {
	IDs             []string `json:"ids"`
	Rounds          int      `json:"rounds"`
	Workers         int      `json:"workers"`
	Timeout         int      `json:"timeout"`
	Tests           []string `json:"tests"`
	ClashURL        string   `json:"clash_url,omitempty"`
	ClashUnified    *bool    `json:"clash_unified,omitempty"`
	Smart           bool     `json:"smart"`
	ClashMaxDelayMS int      `json:"clash_max_delay_ms"`
}

type liveNodeResult struct {
	Node     string    `json:"node"`
	Endpoint string    `json:"endpoint"`
	Round    int       `json:"round"`
	Started  time.Time `json:"started"`
	Outcome  string    `json:"outcome"`
	Status   int       `json:"http_status"`
	TTFB     *float64  `json:"ttfb_ms"`
	Delay    *float64  `json:"delay_ms,omitempty"`
	TLS      *float64  `json:"tls_ms"`
	Total    float64   `json:"total_ms"`
	Exit     int       `json:"curl_exit"`
	Upgraded bool      `json:"websocket_connected"`
	Note     string    `json:"note,omitempty"`
}

type liveNodeExclusion struct {
	Node    string `json:"node"`
	Scope   string `json:"scope"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type liveNodeJob struct {
	PlanRevision     int                 `json:"plan_revision,omitempty"`
	PlanInterval     int                 `json:"plan_interval,omitempty"`
	PlanAuto         bool                `json:"plan_auto,omitempty"`
	Recorded         bool                `json:"recorded,omitempty"`
	StopRequested    bool                `json:"stop_requested,omitempty"`
	PersistenceError string              `json:"persistence_error,omitempty"`
	NodeKeys         map[string]string   `json:"node_keys,omitempty"`
	PlanID           string              `json:"plan_id,omitempty"`
	ID               string              `json:"id"`
	Status           string              `json:"status"`
	Message          string              `json:"message"`
	Started          time.Time           `json:"started"`
	Finished         *time.Time          `json:"finished,omitempty"`
	Options          liveNodeOptions     `json:"options"`
	Nodes            []liveNode          `json:"nodes"`
	Subscriptions    map[string]string   `json:"subscriptions"`
	Results          []liveNodeResult    `json:"results"`
	Done             int                 `json:"done"`
	Total            int                 `json:"total"`
	Cleaned          bool                `json:"cleaned"`
	Skipped          int                 `json:"skipped"`
	Exclusions       []liveNodeExclusion `json:"exclusions"`
}

type liveNodeService struct {
	db           *sql.DB
	ctx          context.Context
	home, binary string
	mu           sync.Mutex
	job          *liveNodeJob
	cancel       context.CancelFunc
	finished     chan struct{}
}

func liveNodeBusy(status string) bool {
	return status == "preparing" || status == "running" || status == "stopping" || status == "interrupted"
}

func (s *liveNodeService) snapshot() *liveNodeJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return nil
	}
	job := *s.job
	job.Nodes = append([]liveNode{}, s.job.Nodes...)
	job.Results = append([]liveNodeResult{}, s.job.Results...)
	job.Exclusions = append([]liveNodeExclusion{}, s.job.Exclusions...)
	return &job
}

func (s *liveNodeService) close() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	done := s.finished
	s.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (s *liveNodeService) register(mux *http.ServeMux) {
	mux.HandleFunc("/nodes", func(w http.ResponseWriter, r *http.Request) {
		page, _ := liveAssets.ReadFile("web/nodes.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
	mux.HandleFunc("/api/nodes/catalog", func(w http.ResponseWriter, r *http.Request) {
		writeLiveJSON(w, discoverLiveNodes(s.home, s.binary).catalog)
	})
	mux.HandleFunc("/api/nodes/job", func(w http.ResponseWriter, r *http.Request) { writeLiveJSON(w, s.snapshot()) })
	mux.HandleFunc("/api/nodes/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
			return
		}
		var options liveNodeOptions
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&options) != nil {
			http.Error(w, "无效的测速参数", http.StatusBadRequest)
			return
		}
		job, code, err := s.startJob(options, "", nil)
		if err != nil {
			http.Error(w, err.Error(), code)
			return
		}
		writeLiveJSON(w, job)
	})
	mux.HandleFunc("/api/nodes/cancel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		s.mu.Lock()
		if s.job != nil && liveNodeBusy(s.job.Status) {
			s.stopJobLocked()
		}
		s.mu.Unlock()
		writeLiveJSON(w, s.snapshot())
	})
	mux.HandleFunc("/api/nodes/export", func(w http.ResponseWriter, r *http.Request) {
		job := s.snapshot()
		if job == nil {
			http.Error(w, "没有测速记录", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="codex-node-tests.csv"`)
		_, _ = w.Write([]byte{0xef, 0xbb, 0xbf})
		writer := csv.NewWriter(w)
		_ = writer.Write([]string{"订阅", "节点", "协议类型", "测试入口", "轮次", "时间", "结果", "HTTP状态（Clash为内核接口状态）", "首响应毫秒", "TLS毫秒", "总耗时毫秒", "WS已升级", "Clash原生延迟毫秒", "智能判定或跳过原因"})
		nodes := map[string]liveNode{}
		for _, node := range job.Nodes {
			nodes[node.ID] = node
		}
		for _, row := range job.Results {
			node := nodes[row.Node]
			text := func(value *float64) string {
				if value == nil {
					return ""
				}
				return strconv.FormatFloat(*value, 'f', 1, 64)
			}
			status, total := strconv.Itoa(row.Status), strconv.FormatFloat(row.Total, 'f', 1, 64)
			if row.Outcome == "skipped" {
				status, total = "", ""
			}
			_ = writer.Write([]string{job.Subscriptions[node.Subscription], node.Name, node.Type, row.Endpoint, strconv.Itoa(row.Round), row.Started.Format(time.RFC3339), row.Outcome, status, text(row.TTFB), text(row.TLS), total, strconv.FormatBool(row.Upgraded), text(row.Delay), row.Note})
		}
		writer.Flush()
	})
}

func (s *liveNodeService) run(ctx context.Context, inv liveNodeInventory, selected []*liveNodeSpec, job *liveNodeJob) {
	var failure error
	var cleanup func()
	root := ""
	defer func() {
		if cleanup != nil {
			cleanup()
		}
		cleaned := root == "" || os.RemoveAll(root) == nil
		s.mu.Lock()
		defer s.mu.Unlock()
		now := time.Now()
		job.Finished, job.Cleaned = &now, cleaned
		switch {
		case ctx.Err() != nil && s.ctx.Err() != nil && !job.StopRequested:
			job.Status, job.Message, job.Finished = "interrupted", "服务停止，已保存完成项；下次启动继续测速", nil
		case ctx.Err() != nil:
			job.Status, job.Message = "cancelled", "测速已停止，已保留完成的结果"
		case failure != nil:
			job.Status, job.Message = "failed", failure.Error()
		default:
			job.Status, job.Message = "completed", "测速完成，已按所选项目分别统计"
			if job.Options.Smart {
				job.Message = fmt.Sprintf("智能测速完成：实测 %d 项，跳过 %d 项", job.Done-job.Skipped, job.Skipped)
			}
		}
		if !cleaned {
			job.Message += "；临时目录清理失败，请关闭占用文件的程序"
		}
		s.saveJobLocked(job)
	}()
	proxies, aliases, err := compileLiveNodeSpecs(selected)
	if err != nil {
		failure = err
		return
	}
	root, err = os.MkdirTemp("", "codex-tps-node-test-")
	if err != nil {
		failure = errors.New("无法创建本机临时测速目录")
		return
	}
	ports, releasePorts, err := reserveLiveNodePorts(len(selected) + 1)
	if err != nil {
		failure = errors.New("无法分配本机临时端口")
		return
	}
	defer releasePorts()
	secretBytes := make([]byte, 24)
	if _, err = rand.Read(secretBytes); err != nil {
		failure = errors.New("无法生成临时内核凭据")
		return
	}
	secret := hex.EncodeToString(secretBytes)
	controller := fmt.Sprintf("127.0.0.1:%d", ports[len(selected)])
	listeners := []map[string]any{}
	for i, node := range selected {
		listeners = append(listeners, map[string]any{"name": fmt.Sprintf("probe-in-%d", i), "type": "http", "listen": "127.0.0.1", "port": ports[i], "proxy": aliases[node.public.ID], "udp": false})
	}
	config := map[string]any{
		"port": 0, "socks-port": 0, "mixed-port": 0, "allow-lan": false, "bind-address": "127.0.0.1",
		"mode": "rule", "log-level": "silent", "external-controller": controller, "secret": secret,
		"ipv6": false, "find-process-mode": "off", "geo-auto-update": false,
		"unified-delay": *job.Options.ClashUnified,
		"tun":           map[string]any{"enable": false}, "dns": map[string]any{"enable": false},
		"profile": map[string]any{"store-selected": false, "store-fake-ip": false},
		"proxies": proxies, "listeners": listeners, "rules": []string{"MATCH,REJECT"},
	}
	data, err := yaml.Marshal(config)
	path := filepath.Join(root, "config.yaml")
	if err != nil || os.WriteFile(path, data, 0600) != nil {
		failure = errors.New("无法写入临时内核配置")
		return
	}
	validateCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	command := exec.CommandContext(validateCtx, inv.binary, "-t", "-d", root, "-f", path)
	hideLiveNodeCommand(command)
	err = command.Run() // No raw Mihomo logs: they can contain credentials.
	cancel()
	if err != nil {
		failure = errors.New("选中节点未通过 Mihomo 配置校验，请刷新 Clash 订阅或减少选择后重试")
		return
	}
	releasePorts()
	cleanup, err = startLiveNodeCore(ctx, inv.binary, root, path, controller, secret, ports[:len(selected)])
	if err != nil {
		failure = err
		return
	}
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	job.Status, job.Message = "running", "正在独立探测；不会切换当前节点"
	s.mu.Unlock()
	probe := func(index int, endpoint string, round int) liveNodeResult {
		if endpoint == "clash" {
			return probeLiveClashNode(ctx, controller, secret, aliases[selected[index].public.ID], selected[index].public.ID, round, job.Options.Timeout, job.Options.ClashURL)
		}
		return probeLiveNode(ctx, inv.curl, root, ports[index], selected[index].public.ID, endpoint, round, job.Options.Timeout)
	}
	completed := liveNodeCompleted(job)
	record := func(row liveNodeResult) {
		s.mu.Lock()
		defer s.mu.Unlock()
		key := liveNodeResultKey(row.Node, row.Endpoint, row.Round)
		for _, existing := range job.Results {
			if liveNodeResultKey(existing.Node, existing.Endpoint, existing.Round) == key {
				return
			}
		}
		s.saveResultLocked(job, row)
		job.Results = append(job.Results, row)
		job.Done = len(job.Results)
		if row.Outcome == "skipped" {
			job.Skipped++
		}
	}
	if job.Options.Smart {
		s.runSmartNodes(ctx, selected, job, probe, record)
		return
	}
	type task struct {
		index, round int
		endpoint     string
	}
	tasks := make(chan task)
	var workers sync.WaitGroup
	for range job.Options.Workers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for task := range tasks {
				if ctx.Err() != nil {
					return
				}
				row := probe(task.index, task.endpoint, task.round)
				if ctx.Err() != nil {
					return
				}
				record(row)
			}
		}()
	}
	// Rotate the first tested node between rounds rather than always giving one
	// subscription the cold-start period. No global selector is touched.
	func() {
		defer close(tasks)
		for round := 1; round <= job.Options.Rounds; round++ {
			for offset := range len(selected) {
				index := (offset + round - 1) % len(selected)
				for _, endpoint := range job.Options.Tests {
					if completed[liveNodeResultKey(selected[index].public.ID, endpoint, round)] {
						continue
					}
					select {
					case tasks <- task{index, round, endpoint}:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	workers.Wait()
}

func probeLiveClashNode(ctx context.Context, controller, secret, alias, node string, round, timeout int, testURL string) liveNodeResult {
	row := liveNodeResult{Node: node, Endpoint: "clash", Round: round, Started: time.Now(), Outcome: "clash_api_error", Exit: -1}
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout+3)*time.Second)
	defer cancel()
	query := url.Values{"timeout": {strconv.Itoa(timeout * 1000)}, "url": {testURL}}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://"+controller+"/proxies/"+url.PathEscape(alias)+"/delay?"+query.Encode(), nil)
	if err != nil {
		return row
	}
	request.Header.Set("Authorization", "Bearer "+secret)
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	row.Total = float64(time.Since(row.Started)) / float64(time.Millisecond)
	if err != nil {
		if requestCtx.Err() != nil {
			row.Outcome = "timeout"
		}
		return row
	}
	defer response.Body.Close()
	row.Status = response.StatusCode
	if response.StatusCode == http.StatusGatewayTimeout {
		row.Outcome = "timeout"
		return row
	}
	if response.StatusCode == http.StatusServiceUnavailable {
		row.Outcome = "clash_failed"
		return row
	}
	if response.StatusCode != http.StatusOK {
		return row
	}
	var result struct {
		Delay float64 `json:"delay"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&result) != nil || result.Delay <= 0 {
		row.Outcome = "invalid_clash_metrics"
		return row
	}
	row.Delay, row.Outcome = &result.Delay, "clash_ok"
	return row
}

func reserveLiveNodePorts(count int) ([]int, func(), error) {
	var listeners []net.Listener
	var ports []int
	var once sync.Once
	release := func() {
		once.Do(func() {
			for _, listener := range listeners {
				_ = listener.Close()
			}
		})
	}
	for range count {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			release()
			return nil, release, err
		}
		listeners = append(listeners, listener)
		ports = append(ports, listener.Addr().(*net.TCPAddr).Port)
	}
	return ports, release, nil
}

func startLiveNodeCore(ctx context.Context, binary, root, path, controller, secret string, ports []int) (func(), error) {
	command := exec.CommandContext(ctx, binary, "-d", root, "-f", path)
	hideLiveNodeCommand(command)
	if command.Start() != nil {
		return nil, errors.New("无法启动独立 Mihomo 内核")
	}
	releaseOwner, err := ownLiveNodeProcess(command)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, errors.New("无法为临时内核建立进程清理范围")
	}
	done := make(chan struct{})
	go func() { _ = command.Wait(); close(done) }()
	var once sync.Once
	cleanup := func() { once.Do(func() { _ = command.Process.Kill(); <-done; releaseOwner() }) }
	client := &http.Client{Timeout: 500 * time.Millisecond, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+controller+"/proxies", nil)
		request.Header.Set("Authorization", "Bearer "+secret)
		response, err := client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				ready := true
				for _, port := range ports {
					connection, dialErr := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
					if dialErr != nil {
						ready = false
						break
					}
					connection.Close()
				}
				if ready {
					return cleanup, nil
				}
			}
		}
		select {
		case <-done:
			cleanup()
			return nil, errors.New("临时内核提前退出")
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
	cleanup()
	return nil, errors.New("临时内核启动超时")
}

func probeLiveNode(ctx context.Context, curl, root string, port int, node, endpoint string, round, timeout int) liveNodeResult {
	row := liveNodeResult{Node: node, Endpoint: endpoint, Round: round, Started: time.Now(), Outcome: "probe_error", Exit: -1}
	dir, err := os.MkdirTemp(root, "request-")
	if err != nil {
		row.Outcome = "local_io_error"
		return row
	}
	defer os.RemoveAll(dir)
	args := []string{"--silent", "--show-error", "--max-time", strconv.Itoa(timeout), "--connect-timeout", strconv.Itoa(min(7, timeout)),
		"--noproxy", "", "--proxy", fmt.Sprintf("http://127.0.0.1:%d", port), "--http1.1", "--max-filesize", "1048576",
		"--output", filepath.Join(dir, "body"), "--dump-header", filepath.Join(dir, "headers"), "--write-out", "%{json}"}
	if endpoint == "websocket" {
		key := make([]byte, 16)
		if _, err := rand.Read(key); err != nil {
			row.Outcome = "local_random_error"
			return row
		}
		args = append(args, "--header", "Connection: Upgrade", "--header", "Upgrade: websocket", "--header", "Sec-WebSocket-Version: 13", "--header", "Sec-WebSocket-Key: "+base64.StdEncoding.EncodeToString(key))
	}
	args = append(args, liveNodeEndpoints[endpoint])
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout+5)*time.Second)
	defer cancel()
	command := exec.CommandContext(requestCtx, curl, args...)
	hideLiveNodeCommand(command)
	output, err := command.Output()
	if err == nil {
		row.Exit = 0
	} else {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			row.Exit = exit.ExitCode()
		}
	}
	var metrics struct {
		Status int     `json:"response_code"`
		Start  float64 `json:"time_starttransfer"`
		TLS    float64 `json:"time_appconnect"`
		Total  float64 `json:"time_total"`
	}
	if json.Unmarshal(output, &metrics) != nil {
		row.Outcome = "invalid_curl_metrics"
		return row
	}
	row.Status, row.Total = metrics.Status, metrics.Total*1000
	if metrics.Start > 0 {
		value := metrics.Start * 1000
		row.TTFB = &value
	}
	if metrics.TLS > 0 {
		value := metrics.TLS * 1000
		row.TLS = &value
	}
	body, _ := os.ReadFile(filepath.Join(dir, "body"))
	headers, _ := os.ReadFile(filepath.Join(dir, "headers"))
	text := strings.ToLower(string(body))
	switch {
	case endpoint == "websocket" && row.Status == 101:
		row.Outcome, row.Upgraded = "upgrade_accepted", true
	case requestCtx.Err() != nil:
		row.Outcome = "timeout"
	case row.Exit != 0:
		row.Outcome = map[int]string{28: "timeout", 7: "connect_error", 35: "tls_error", 60: "certificate_error", 56: "connection_error", 63: "response_too_large"}[row.Exit]
		if row.Outcome == "" {
			row.Outcome = "curl_error"
		}
	case strings.Contains(strings.ToLower(string(headers)), "cf-mitigated: challenge") || strings.Contains(text, "just a moment") || strings.Contains(text, "cf-chl-"):
		row.Outcome = "waf_challenge"
	case row.Status == 401 && json.Valid(body) && (strings.HasPrefix(strings.TrimSpace(string(body)), "{") || strings.HasPrefix(strings.TrimSpace(string(body)), "[")):
		row.Outcome = "auth_reached"
	default:
		row.Outcome = "http_" + strconv.Itoa(row.Status)
	}
	return row
}
