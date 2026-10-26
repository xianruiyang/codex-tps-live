//go:build windows

package codextps

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func captureProcessAlive(pid int, created int64) bool {
	return captureProcessNamedAlive(pid, "codex-tps-capture.exe", created)
}

func captureBackendAlive(pid int, created int64) bool {
	return captureProcessNamedAlive(pid, "codex.exe", created)
}

func captureProcessNamedAlive(pid int, name string, created int64) bool {
	if pid <= 0 || created <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	state, err := windows.WaitForSingleObject(h, 0)
	if err != nil || state != uint32(windows.WAIT_TIMEOUT) {
		return false
	}
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if windows.QueryFullProcessImageName(h, 0, &buffer[0], &size) != nil {
		return false
	}
	if !strings.EqualFold(filepath.Base(windows.UTF16ToString(buffer[:size])), name) {
		return false
	}
	var creation, exit, kernelTime, userTime windows.Filetime
	return windows.GetProcessTimes(h, &creation, &exit, &kernelTime, &userTime) == nil && creation.Nanoseconds() == created
}
