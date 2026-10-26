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
	"unsafe"

	"golang.org/x/sys/windows"
)

const captureTarget = "codex_api::responses_websocket_timing"

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
		return cfg, len(apps) > 0, errors.New("请先正常打开 Windows Codex，再执行一次计时采集准备")
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

func featureCommand(cfg captureConfig, action string) *exec.Cmd {
	args := []string{"features", action}
	if action != "list" {
		args = append(args, "runtime_metrics")
	}
	cmd := exec.Command(cfg.CodexCLI, args...)
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
	output, err := featureCommand(cfg, "list").Output()
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

func writeRuntimeFeature(cfg captureConfig, enabled bool) error {
	action := "disable"
	if enabled {
		action = "enable"
	}
	cmd := featureCommand(cfg, action)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func prepareCapture() error {
	cfg, _, err := discoverCapture()
	if err != nil {
		return err
	}
	old, oldErr := loadCaptureConfig()
	if oldErr == nil && old.Enabled && old.PreviousSaved && strings.EqualFold(old.CodexHome, cfg.CodexHome) {
		cfg.PreviousSaved, cfg.PreviousMetric = true, old.PreviousMetric
	} else {
		previous, readErr := readRuntimeFeature(cfg)
		if readErr != nil {
			return readErr
		}
		cfg.PreviousSaved, cfg.PreviousMetric = true, previous
	}
	cfg.Enabled = true
	if err = saveCaptureConfig(cfg); err != nil {
		return err
	}
	if err = writeRuntimeFeature(cfg, true); err != nil {
		cfg.Enabled = false
		_ = saveCaptureConfig(cfg)
		return err
	}
	fmt.Println("已准备：只启用 features.runtime_metrics，未修改认证、模型、API 地址或全局环境。")
	fmt.Println("当前 Codex 不会被关闭。结束工作后退出 Codex，再用“启动Codex计时采集.cmd”启动。")
	fmt.Println("撤销入口：codex-tps-capture.exe --capture-disable")
	return nil
}

func launchCapture() error {
	_, running, _ := discoverCapture()
	if running {
		return prepareCapture()
	}
	cfg, err := loadCaptureConfig()
	if err != nil {
		return fmt.Errorf("请在 Codex 正常运行时先执行一次计时准备: %w", err)
	}
	if !cfg.Enabled {
		return errors.New("采集已撤销；请先正常打开 Codex，再执行计时准备")
	}
	for _, path := range []string{cfg.CodexCLI, cfg.CodexApp} {
		if _, err = os.Stat(path); err != nil {
			return fmt.Errorf("Codex 可能已更新，请正常打开新版本后重新准备采集: %w", err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(cfg.CodexApp)
	cmd.Dir = filepath.Dir(cfg.CodexApp)
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if !strings.EqualFold(key, "RUST_LOG") && !strings.EqualFold(key, "CODEX_CLI_PATH") && !strings.EqualFold(key, "CODEX_TPS_ORIGINAL_CLI") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	filter := strings.TrimSpace(os.Getenv("RUST_LOG"))
	if filter == "" {
		filter = "warn"
	}
	cmd.Env = append(cmd.Env, "RUST_LOG="+filter+","+captureTarget+"=trace", "CODEX_CLI_PATH="+self, "CODEX_TPS_ORIGINAL_CLI="+cfg.CodexCLI)
	if err = cmd.Start(); err != nil {
		return err
	}
	fmt.Println("已启动 Codex 定向计时采集；标准输入/输出直接继承，原版 CLI 处理全部请求。")
	return cmd.Process.Release()
}

func disableCapture() error {
	cfg, err := loadCaptureConfig()
	if err != nil {
		return err
	}
	current, err := readRuntimeFeature(cfg)
	if err != nil {
		return err
	}
	if cfg.PreviousSaved && current != nil && *current {
		previous := cfg.PreviousMetric != nil && *cfg.PreviousMetric
		if err = writeRuntimeFeature(cfg, previous); err != nil {
			return err
		}
	}
	cfg.Enabled = false
	if err = saveCaptureConfig(cfg); err == nil {
		fmt.Println("已撤销采集设置；下次正常启动 Codex 即恢复普通启动方式，当前工作不受影响。")
	}
	return err
}

func captureStatus() error {
	cfg, err := loadCaptureConfig()
	if err != nil {
		return err
	}
	dir, _ := captureDir()
	fmt.Printf("采集准备：%t\n本地目录：%s\n", cfg.Enabled, dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "ws-timing-") && strings.HasSuffix(entry.Name(), ".jsonl") {
			count++
		}
	}
	fmt.Printf("计时记录文件：%d；只有实际收到服务端 timing 时才会创建。\n", count)
	return nil
}
