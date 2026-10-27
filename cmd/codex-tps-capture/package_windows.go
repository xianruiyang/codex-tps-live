//go:build windows

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type capturePackageRequest struct {
	Config    captureConfig
	Env       []string
	Self      string
	LaunchID  string
	Probe     bool
	ProbePath string
}

type capturePackageInfo struct {
	Family   string `json:"family"`
	CLI      string `json:"cli"`
	Original string `json:"original"`
	Filter   string `json:"filter"`
	LaunchID string `json:"launch_id"`
}

type capturePackageResult struct {
	PID   int                `json:"pid,omitempty"`
	Info  capturePackageInfo `json:"info"`
	Error string             `json:"error,omitempty"`
}

func currentCapturePackageInfo() capturePackageInfo {
	info := capturePackageInfo{CLI: os.Getenv("CODEX_CLI_PATH"), Original: os.Getenv("CODEX_TPS_ORIGINAL_CLI"), Filter: os.Getenv("RUST_LOG"), LaunchID: os.Getenv("CODEX_TPS_CAPTURE_LAUNCH_ID")}
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCurrentPackageFamilyName")
	var size uint32
	code, _, _ := proc.Call(uintptr(unsafe.Pointer(&size)), 0)
	if code == uintptr(windows.ERROR_INSUFFICIENT_BUFFER) && size > 0 && size < 32768 {
		buffer := make([]uint16, size)
		code, _, _ = proc.Call(uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&buffer[0])))
		if code == 0 {
			info.Family = windows.UTF16ToString(buffer)
		}
	}
	return info
}

func captureDesktopEnv(cfg captureConfig, self, launchID string) []string {
	var result []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !strings.EqualFold(key, "RUST_LOG") && !strings.EqualFold(key, "CODEX_CLI_PATH") && !strings.EqualFold(key, "CODEX_HOME") && !strings.EqualFold(key, "CODEX_TPS_ORIGINAL_CLI") && !strings.EqualFold(key, "CODEX_TPS_CAPTURE_LAUNCH_ID") {
			result = append(result, item)
		}
	}
	filter := strings.TrimSpace(os.Getenv("RUST_LOG"))
	if filter == "" {
		filter = "warn"
	}
	return append(result, "RUST_LOG="+filter+","+captureTarget+"=trace", "CODEX_CLI_PATH="+self, "CODEX_HOME="+cfg.CodexHome, "CODEX_TPS_ORIGINAL_CLI="+cfg.CodexCLI, "CODEX_TPS_CAPTURE_LAUNCH_ID="+launchID)
}

func capturePSLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

// AppModel does not inherit the caller's environment. Start only our short-lived
// helper inside the registered package, then hand it the launch environment in
// memory over a one-use authenticated loopback connection. No registry changes,
// package debugging settings, ACL edits or original executable replacement.
func capturePackageDesktop(cfg captureConfig, launchID string, probe bool) (capturePackageResult, error) {
	var result capturePackageResult
	if cfg.PackageFamily == "" || cfg.PackageAppID == "" {
		return result, errors.New("缺少已注册的 Windows 应用包入口，未直接执行 WindowsApps 内的 EXE")
	}
	self, err := os.Executable()
	if err != nil {
		return result, err
	}
	var probePath string
	if probe {
		dir, dirErr := captureDir()
		if dirErr != nil {
			return result, dirErr
		}
		if err = os.MkdirAll(dir, 0700); err != nil {
			return result, err
		}
		probeDir, dirErr := os.MkdirTemp(dir, ".capture-check-")
		if dirErr != nil {
			return result, dirErr
		}
		probePath = filepath.Join(probeDir, "visibility")
		defer os.Remove(probeDir)
		defer os.Remove(probePath)
	}
	systemDir, err := windows.GetSystemDirectory()
	if err != nil {
		return result, err
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return result, err
	}
	defer listener.Close()
	deadline := time.Now().Add(20 * time.Second)
	_ = listener.SetDeadline(deadline)
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		return result, err
	}
	key := hex.EncodeToString(secret[:])
	arguments := "--capture-package-bridge " + listener.Addr().String() + " " + key
	// The cmdlet otherwise assigns identity only to the bridge itself. Its
	// documented PreventBreakaway switch keeps our child in the same context.
	script := "$ErrorActionPreference='Stop'; Invoke-CommandInDesktopPackage -PreventBreakaway -PackageFamilyName " + capturePSLiteral(cfg.PackageFamily) + " -AppId " + capturePSLiteral(cfg.PackageAppID) + " -Command " + capturePSLiteral(self) + " -Args " + capturePSLiteral(arguments)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	// Appx is a Windows inbox component hosted by Windows PowerShell.
	cmd := exec.CommandContext(ctx, filepath.Join(systemDir, "WindowsPowerShell", "v1.0", "powershell.exe"), "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Dir = cfg.CodexHome
	// Unlike pwsh itself, an intermediate native launcher does not remove PS7
	// module paths. Let the Windows inbox host construct its own module path.
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !strings.EqualFold(key, "PSModulePath") {
			cmd.Env = append(cmd.Env, item)
		}
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err = cmd.Start(); err != nil {
		return result, fmt.Errorf("无法启动 Windows Appx 桥接: %w", err)
	}
	finished := make(chan error, 1)
	go func() {
		waitErr := cmd.Wait()
		finished <- waitErr
		if waitErr != nil {
			listener.Close()
		}
	}()
	conn, err := listener.AcceptTCP()
	if err != nil {
		cancel()
		waitErr := <-finished
		return result, fmt.Errorf("Windows 应用包桥接未接通（未启动 Codex）: %v；启动器: %v；%s", err, waitErr, strings.TrimSpace(output.String()))
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	var hello struct{ Key string }
	if err = json.NewDecoder(io.LimitReader(conn, 1024)).Decode(&hello); err != nil || subtle.ConstantTimeCompare([]byte(hello.Key), []byte(key)) != 1 {
		return result, errors.New("Windows 应用包桥接身份校验失败，未下发启动信息")
	}
	request := capturePackageRequest{Config: cfg, Env: captureDesktopEnv(cfg, self, launchID), Self: self, LaunchID: launchID, Probe: probe, ProbePath: probePath}
	if err = json.NewEncoder(conn).Encode(request); err != nil {
		return result, err
	}
	if err = json.NewDecoder(io.LimitReader(conn, 65536)).Decode(&result); err != nil {
		return result, fmt.Errorf("Windows 应用包桥接未返回启动结果: %w", err)
	}
	if result.Error != "" {
		return result, errors.New(result.Error)
	}
	if result.Info.Family != cfg.PackageFamily {
		return result, fmt.Errorf("桥接未获得目标 Windows 应用包身份（预期 %q，实际 %q）", cfg.PackageFamily, result.Info.Family)
	}
	if probe && (!strings.EqualFold(result.Info.CLI, self) || !strings.EqualFold(result.Info.Original, cfg.CodexCLI) || result.Info.LaunchID != launchID || !strings.Contains(result.Info.Filter, captureTarget+"=trace")) {
		return result, errors.New("包身份诊断已启动，但临时采集环境没有完整传递；未启动 Codex")
	}
	if probe {
		data, readErr := os.ReadFile(probePath)
		if readErr != nil || string(data) != launchID {
			return result, errors.New("应用包内写入的临时诊断记录无法在普通监控侧正确读取；未启动 Codex")
		}
		if err = json.NewEncoder(conn).Encode(true); err != nil {
			return result, err
		}
	}
	return result, nil
}

func runCapturePackageBridge(args []string) error {
	if len(args) != 2 {
		return errors.New("应用包桥接参数不完整")
	}
	address, err := net.ResolveTCPAddr("tcp4", args[0])
	if err != nil || address.IP == nil || !address.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		return errors.New("应用包桥接仅允许本机临时通道")
	}
	conn, err := net.DialTimeout("tcp4", address.String(), 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if err = json.NewEncoder(conn).Encode(struct{ Key string }{args[1]}); err != nil {
		return err
	}
	var request capturePackageRequest
	if err = json.NewDecoder(io.LimitReader(conn, 4<<20)).Decode(&request); err != nil {
		return err
	}
	result := capturePackageResult{Info: currentCapturePackageInfo()}
	if result.Info.Family != request.Config.PackageFamily || result.Info.Family == "" {
		result.Error = "Windows 未赋予桥接目标应用包身份，未启动 Codex"
	} else if request.Probe {
		defer os.Remove(filepath.Dir(request.ProbePath))
		defer os.Remove(request.ProbePath)
		if err = os.WriteFile(request.ProbePath, []byte(request.LaunchID), 0600); err != nil {
			result.Error = fmt.Sprintf("无法在独立临时诊断目录写入记录（未启动 Codex）: %v", err)
			return json.NewEncoder(conn).Encode(result)
		}
		// Check the actual registered desktop image's execute access without
		// creating a Codex process. A dummy helper alone cannot prove this.
		image, pathErr := windows.UTF16PtrFromString(request.Config.CodexApp)
		if pathErr != nil {
			return pathErr
		}
		handle, accessErr := windows.CreateFile(image, windows.GENERIC_EXECUTE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
		if accessErr != nil {
			result.Error = fmt.Sprintf("包身份已获得，但无法取得已注册桌面 EXE 的执行访问（未启动 Codex）: %v", accessErr)
			return json.NewEncoder(conn).Encode(result)
		}
		windows.CloseHandle(handle)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		child := exec.CommandContext(ctx, request.Self, "--capture-package-info")
		child.Env = request.Env
		child.Dir = request.Config.CodexHome
		child.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		data, childErr := child.Output()
		if childErr != nil {
			result.Error = fmt.Sprintf("包身份子进程诊断失败（未启动 Codex）: %v", childErr)
		} else if err = json.Unmarshal(data, &result.Info); err != nil {
			result.Error = fmt.Sprintf("包身份子进程诊断结果无效: %v", err)
		}
	} else if _, running, discoverErr := discoverCapture(); running || (discoverErr != nil && !errors.Is(discoverErr, errCaptureNotRunning)) {
		result.Error = "启动前检测到 Codex 已运行或无法可靠确认进程状态；未接管或重启任何进程"
	} else {
		child := exec.Command(request.Config.CodexApp)
		child.Dir = request.Config.CodexHome
		child.Env = request.Env
		if err = child.Start(); err != nil {
			result.Error = fmt.Sprintf("应用包身份已确认，但桌面进程创建失败: %v", err)
		} else {
			result.PID = child.Process.Pid
			_ = child.Process.Release()
		}
	}
	if err = json.NewEncoder(conn).Encode(result); err != nil {
		return err
	}
	if request.Probe && result.Error == "" {
		var acknowledged bool
		return json.NewDecoder(io.LimitReader(conn, 64)).Decode(&acknowledged)
	}
	return nil
}

func checkCapturePackage() error {
	cfg, err := discoverInstalledCapture(true)
	if err != nil {
		return err
	}
	_, err = capturePackageDesktop(cfg, fmt.Sprintf("check-%d", time.Now().UnixNano()), true)
	if err != nil {
		return err
	}
	fmt.Println("Windows 应用包身份、桌面 EXE 执行访问、临时环境传递和采集目录可见性检查通过。仅运行了诊断子进程，未启动/关闭 Codex，未修改 Codex 配置或全局环境。")
	fmt.Println("此结果不代表桌面与后端已经接通，也不代表已收到 WebSocket 计时。")
	return nil
}

func writeCaptureLaunch(stage string, pid int, launchErr error) {
	dir, err := captureDir()
	if err == nil {
		err = os.MkdirAll(dir, 0700)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "无法保存采集启动诊断:", err)
		return
	}
	message := ""
	if launchErr != nil {
		message = launchErr.Error()
	}
	data, err := json.MarshalIndent(struct {
		At    time.Time `json:"at"`
		Stage string    `json:"stage"`
		PID   int       `json:"desktop_pid,omitempty"`
		Error string    `json:"error,omitempty"`
	}{time.Now().UTC(), stage, pid, message}, "", "  ")
	if err == nil {
		err = atomicCaptureWrite(filepath.Join(dir, "last-launch.json"), append(data, '\n'))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "无法保存采集启动诊断:", err)
	}
}
