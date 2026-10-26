//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const captureTarget = "codex_api::responses_websocket_timing"

var errCaptureNotRunning = errors.New("未检测到正在运行的 Codex")

type captureConfig struct {
	CodexCLI       string `json:"codex_cli"`
	CodexApp       string `json:"codex_app"`
	CodexHome      string `json:"codex_home"`
	Enabled        bool   `json:"enabled"`
	PreviousSaved  bool   `json:"previous_saved"`
	PreviousMetric *bool  `json:"previous_runtime_metrics"`
}

func captureDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "CodexTPSLive", "timing"), nil
}

func loadCaptureConfig() (captureConfig, error) {
	var cfg captureConfig
	dir, err := captureDir()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "capture.json"))
	if err == nil {
		err = json.Unmarshal(data, &cfg)
	}
	return cfg, err
}

func saveCaptureConfig(cfg captureConfig) error {
	dir, err := captureDir()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicCaptureWrite(filepath.Join(dir, "capture.json"), append(data, '\n'))
}

func atomicCaptureWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".codex-tps-timing-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

type captureProcess struct {
	pid, parent uint32
	path        string
}

func discoverCapture() (captureConfig, bool, error) {
	var cfg captureConfig
	handle, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return cfg, false, err
	}
	defer windows.CloseHandle(handle)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	processes := map[uint32]captureProcess{}
	apps := map[uint32]string{}
	unresolvedApp := false
	for err = windows.Process32First(handle, &entry); err == nil; err = windows.Process32Next(handle, &entry) {
		name := strings.ToLower(windows.UTF16ToString(entry.ExeFile[:]))
		process := captureProcess{pid: entry.ProcessID, parent: entry.ParentProcessID}
		if name == "chatgpt.exe" || name == "codex.exe" {
			h, queryErr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, entry.ProcessID)
			if queryErr == nil {
				buffer := make([]uint16, 32768)
				size := uint32(len(buffer))
				if windows.QueryFullProcessImageName(h, 0, &buffer[0], &size) == nil {
					process.path = windows.UTF16ToString(buffer[:size])
				}
				windows.CloseHandle(h)
			}
			if name == "chatgpt.exe" && strings.Contains(strings.ToLower(process.path), "openai.codex_") {
				apps[process.pid] = process.path
			}
			if name == "chatgpt.exe" && process.path == "" {
				unresolvedApp = true
			}
		}
		processes[process.pid] = process
	}
	type pair struct {
		app, cli string
		depth    int
	}
	var pairs []pair
	for _, process := range processes {
		if !strings.EqualFold(filepath.Base(process.path), "codex.exe") || strings.Contains(strings.ToLower(process.path), "windowsapps") {
			continue
		}
		parent := process.parent
		for depth := 1; depth <= 8 && parent != 0; depth++ {
			if app := apps[parent]; app != "" {
				pairs = append(pairs, pair{app, process.path, depth})
				break
			}
			next, exists := processes[parent]
			if !exists || next.parent == parent {
				break
			}
			parent = next.parent
		}
	}
	if len(pairs) == 0 {
		if len(apps) > 0 || unresolvedApp {
			return cfg, true, errors.New("桌面进程正在运行，但未能确认其后端；未接管或修改当前进程")
		}
		return cfg, false, errCaptureNotRunning
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].depth < pairs[j].depth })
	first := pairs[0]
	for _, candidate := range pairs[1:] {
		if candidate.depth == first.depth && (candidate.app != first.app || candidate.cli != first.cli) {
			return cfg, true, errors.New("发现多个不同版本的 Codex 后端，请保留需要采集的桌面实例后重新准备")
		}
	}
	cfg.CodexApp, cfg.CodexCLI = first.app, first.cli
	cfg.CodexHome = os.Getenv("CODEX_HOME")
	if cfg.CodexHome == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return cfg, true, homeErr
		}
		cfg.CodexHome = filepath.Join(home, ".codex")
	}
	cfg.CodexHome, err = filepath.Abs(cfg.CodexHome)
	return cfg, true, err
}

func featureCommand(cfg captureConfig) *exec.Cmd {
	cmd := exec.Command(cfg.CodexCLI, "features", "list")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Dir = cfg.CodexHome
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !strings.EqualFold(key, "CODEX_HOME") {
			cmd.Env = append(cmd.Env, item)
		}
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+cfg.CodexHome)
	return cmd
}

func readRuntimeFeature(cfg captureConfig) (*bool, error) {
	output, err := featureCommand(cfg).Output()
	if err != nil {
		return nil, fmt.Errorf("原版 Codex 无法读取 feature 状态，未修改配置: %w", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "runtime_metrics" {
			last := fields[len(fields)-1]
			if last == "true" || last == "false" {
				enabled := last == "true"
				return &enabled, nil
			}
		}
	}
	return nil, errors.New("当前 Codex 没有 runtime_metrics 功能，未修改配置")
}

func savePreparedCapture(cfg captureConfig) error {
	old, err := loadCaptureConfig()
	if err == nil && old.PreviousSaved && strings.EqualFold(old.CodexHome, cfg.CodexHome) {
		// Retain only the rollback responsibility inherited from older builds.
		cfg.PreviousSaved, cfg.PreviousMetric = true, old.PreviousMetric
	}
	cfg.Enabled = true
	return saveCaptureConfig(cfg)
}

func prepareCapture() error {
	cfg, running, err := discoverCapture()
	if !running && errors.Is(err, errCaptureNotRunning) {
		cfg, err = discoverInstalledCapture(true)
	}
	if err != nil {
		return err
	}
	if err = savePreparedCapture(cfg); err != nil {
		return err
	}
	fmt.Println("已保存采集器自己的路径信息；未修改 Codex 配置或当前进程。")
	fmt.Println("此操作不代表采集已经运行。原生功能仅在将来的采集进程中通过 --enable runtime_metrics 启用。")
	return nil
}

func launchCapture() error {
	_, running, err := discoverCapture()
	if running {
		fmt.Println("检测到 Codex 正在运行；本次仅查询状态，不修改设置、不接管或重启现有进程。")
		return captureStatus()
	}
	if err != nil && !errors.Is(err, errCaptureNotRunning) {
		return err
	}
	cfg, err := discoverInstalledCapture(true)
	if err != nil {
		return err
	}
	// Read-only compatibility check before launching the desktop.
	if _, err = readRuntimeFeature(cfg); err != nil {
		return err
	}
	if err = savePreparedCapture(cfg); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	launchID := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	cmd := exec.Command(cfg.CodexApp)
	cmd.Dir = filepath.Dir(cfg.CodexApp)
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if !strings.EqualFold(key, "RUST_LOG") && !strings.EqualFold(key, "CODEX_CLI_PATH") && !strings.EqualFold(key, "CODEX_TPS_ORIGINAL_CLI") && !strings.EqualFold(key, "CODEX_TPS_CAPTURE_LAUNCH_ID") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	filter := strings.TrimSpace(os.Getenv("RUST_LOG"))
	if filter == "" {
		filter = "warn"
	}
	cmd.Env = append(cmd.Env, "RUST_LOG="+filter+","+captureTarget+"=trace", "CODEX_CLI_PATH="+self, "CODEX_TPS_ORIGINAL_CLI="+cfg.CodexCLI, "CODEX_TPS_CAPTURE_LAUNCH_ID="+launchID)
	if err = cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	dir, err := captureDir()
	if err != nil {
		return err
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		var state captureRunState
		if data, readErr := os.ReadFile(filepath.Join(dir, "active.json")); readErr == nil && json.Unmarshal(data, &state) == nil && state.LaunchID == launchID && captureStateAlive(state) {
			fmt.Println("已确认采集器与原版后端启动；尚未证明收到服务端计时，更不代表已测得客户端逐次 TTFT。")
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("已尝试启动桌面程序，但 5 秒内未确认采集后端接通；未结束 Codex，也未修改其持久配置。可稍后查询采集状态")
}

func disableCapture() error {
	cfg, err := loadCaptureConfig()
	if err != nil {
		return err
	}
	cfg.Enabled = false
	if err = saveCaptureConfig(cfg); err != nil {
		return err
	}
	fmt.Println("已停用将来的可选采集准备；未修改 Codex 配置，未关闭任何进程。")
	if cfg.PreviousSaved {
		fmt.Println("旧版曾保存持久功能设置的恢复信息。本次不自动写回，以免覆盖之后的手动设置；信息继续保留。")
	}
	return nil
}

func captureStatus() error {
	cfg, err := loadCaptureConfig()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Printf("采集器本地准备信息：%t（不代表已运行）\n", cfg.Enabled)
	dir, err := captureDir()
	if err != nil {
		return err
	}
	var state captureRunState
	data, err := os.ReadFile(filepath.Join(dir, "active.json"))
	if os.IsNotExist(err) {
		fmt.Println("未发现采集后端启动记录；客户端逐次 TTFT 尚未验证支持。")
		return nil
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("采集状态文件无效: %w", err)
	}
	alive := captureStateAlive(state)
	if !alive {
		var last captureRunState
		if data, lastErr := os.ReadFile(filepath.Join(dir, "last-run.json")); lastErr == nil && json.Unmarshal(data, &last) == nil && last.PID == state.PID && last.WrapperCreated == state.WrapperCreated && !last.UpdatedAt.Before(state.UpdatedAt) {
			state = last
		}
	}
	fmt.Printf("采集/后端进程仍存活：%t；计时事件 %d，已保存 %d，解析失败 %d，未知结构 %d，上下文缺失 %d，写盘错误 %d，丢块 %d，超长行 %d。\n", alive, state.Events, state.Records, state.Malformed, state.UnknownSchema, state.IncompleteContext, state.WriteErrors, state.DroppedChunks, state.OversizeLines)
	if alive && (state.UpdatedAt.IsZero() || time.Since(state.UpdatedAt) > 5*time.Second) {
		fmt.Println("状态心跳未更新，不能确认日志采集健康。")
	}
	if state.LastError != "" || state.Warning != "" || state.FlushIncomplete {
		fmt.Printf("诊断代码：%s %s；退出写入未完成：%t\n", state.LastError, state.Warning, state.FlushIncomplete)
	}
	fmt.Println("服务端记录仅用于诊断，不用于客户端 TTFT 或实际 TPS。")
	return nil
}
