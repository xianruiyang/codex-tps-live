//go:build windows

package codextps

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func captureProcessAlive(pid int) bool {
	if pid <= 0 {
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
	return strings.EqualFold(filepath.Base(windows.UTF16ToString(buffer[:size])), "codex-tps-capture.exe")
}
