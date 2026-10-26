//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func executableExists(path, base string) bool {
	if !filepath.IsAbs(path) || !strings.EqualFold(filepath.Base(path), base) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func captureHome() (string, error) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(user, ".codex")
	}
	return filepath.Abs(home)
}

// Query current-user package registration, not running processes or a guessed
// WindowsApps version directory. This path also works before Codex is started.
func installedCodexApps() ([]string, error) {
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	packages := kernel.NewProc("GetPackagesByPackageFamily")
	packagePath := kernel.NewProc("GetPackagePathByFullName")
	if err := packages.Find(); err != nil {
		return nil, err
	}
	if err := packagePath.Find(); err != nil {
		return nil, err
	}
	families := map[string]bool{"OpenAI.Codex_2p2nqsd0c76g0": true}
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Classes\Local Settings\Software\Microsoft\Windows\CurrentVersion\AppModel\Repository\Families`, registry.ENUMERATE_SUB_KEYS)
	if err == nil {
		names, readErr := key.ReadSubKeyNames(-1)
		key.Close()
		if readErr == nil {
			for _, name := range names {
				if strings.HasPrefix(strings.ToLower(name), "openai.codex_") {
					families[name] = true
				}
			}
		}
	}
	apps := map[string]bool{}
	for family := range families {
		familyUTF16, _ := windows.UTF16PtrFromString(family)
		var count, size uint32
		code, _, _ := packages.Call(uintptr(unsafe.Pointer(familyUTF16)), uintptr(unsafe.Pointer(&count)), 0, uintptr(unsafe.Pointer(&size)), 0)
		if code != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) || count == 0 || count > 64 || size == 0 || size > 1<<20 {
			continue
		}
		names, buffer := make([]*uint16, count), make([]uint16, size)
		code, _, _ = packages.Call(uintptr(unsafe.Pointer(familyUTF16)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&names[0])), uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&buffer[0])))
		if code != 0 {
			continue
		}
		for _, name := range names {
			if name == nil {
				continue
			}
			var pathSize uint32
			code, _, _ = packagePath.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&pathSize)), 0)
			if code != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) || pathSize == 0 || pathSize > 32768 {
				continue
			}
			path := make([]uint16, pathSize)
			code, _, _ = packagePath.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&pathSize)), uintptr(unsafe.Pointer(&path[0])))
			if code == 0 {
				app := filepath.Join(windows.UTF16ToString(path), "app", "ChatGPT.exe")
				if executableExists(app, "ChatGPT.exe") {
					apps[app] = true
				}
			}
		}
		runtime.KeepAlive(buffer)
	}
	result := make([]string, 0, len(apps))
	for app := range apps {
		result = append(result, app)
	}
	return result, nil
}

func discoverInstalledCapture(useSaved bool) (captureConfig, error) {
	var cfg captureConfig
	apps, err := installedCodexApps()
	if err != nil {
		return cfg, fmt.Errorf("无法查询当前用户的 Codex 安装注册信息: %w", err)
	}
	if len(apps) != 1 {
		return cfg, fmt.Errorf("发现 %d 个可用 Codex 桌面安装，无法唯一确定目标；未启动程序或修改配置", len(apps))
	}
	cfg.CodexApp = apps[0]
	old, oldErr := loadCaptureConfig()
	if useSaved && oldErr == nil && strings.EqualFold(old.CodexApp, cfg.CodexApp) && executableExists(old.CodexCLI, "codex.exe") {
		cfg.CodexCLI = old.CodexCLI
	} else {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return cfg, errors.New("无法定位当前用户的 LOCALAPPDATA，未启动程序")
		}
		paths, globErr := filepath.Glob(filepath.Join(base, "OpenAI", "Codex", "bin", "*", "codex.exe"))
		if globErr != nil {
			return cfg, globErr
		}
		var candidates []string
		for _, path := range paths {
			if executableExists(path, "codex.exe") {
				candidates = append(candidates, path)
			}
		}
		if len(candidates) != 1 {
			return cfg, fmt.Errorf("发现 %d 个原版 CLI 候选，缺少可靠的版本选择依据；未按修改时间猜选，也未启动 Codex", len(candidates))
		}
		cfg.CodexCLI = candidates[0]
	}
	cfg.CodexHome, err = captureHome()
	return cfg, err
}

func capturePIDIdentity(pid int, expected string) int64 {
	if pid <= 0 {
		return 0
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(handle)
	state, err := windows.WaitForSingleObject(handle, 0)
	if err != nil || state != uint32(windows.WAIT_TIMEOUT) {
		return 0
	}
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size) != nil || !strings.EqualFold(filepath.Base(windows.UTF16ToString(buffer[:size])), expected) {
		return 0
	}
	var created, exited, kernelTime, userTime windows.Filetime
	if windows.GetProcessTimes(handle, &created, &exited, &kernelTime, &userTime) != nil {
		return 0
	}
	return created.Nanoseconds()
}

func captureStateAlive(state captureRunState) bool {
	return state.WrapperCreated > 0 && state.BackendCreated > 0 && capturePIDIdentity(state.PID, "codex-tps-capture.exe") == state.WrapperCreated && capturePIDIdentity(state.CodexPID, "codex.exe") == state.BackendCreated
}
