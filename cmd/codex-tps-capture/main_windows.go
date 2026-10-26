//go:build windows

// This optional launcher preserves the original CLI's arguments and inherits
// stdin/stdout handles directly. Only stderr gets a bounded, lossy side copy.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func main() {
	var err error
	if len(os.Args) == 2 {
		switch os.Args[1] {
		case "--capture-setup":
			err = prepareCapture()
		case "--capture-launch":
			err = launchCapture()
		case "--capture-disable":
			err = disableCapture()
		case "--capture-status":
			err = captureStatus()
		default:
			os.Exit(runOriginal())
		}
	} else {
		os.Exit(runOriginal())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runOriginal() int {
	cfg, err := loadCaptureConfig()
	if err != nil {
		// The launch-time original path also lives in this process environment.
		// Removing the optional capture cache must not break an already launched
		// desktop: continue with the original CLI and no collection.
		cfg.CodexCLI = os.Getenv("CODEX_TPS_ORIGINAL_CLI")
		if cfg.CodexCLI == "" {
			fmt.Fprintln(os.Stderr, "Codex timing capture: 请先在 Codex 运行时执行 --capture-setup:", err)
			return 1
		}
	}
	self, _ := os.Executable()
	if strings.EqualFold(self, cfg.CodexCLI) {
		fmt.Fprintln(os.Stderr, "Codex timing capture: 原版 CLI 路径不能指向采集器自身")
		return 1
	}
	cmd := exec.Command(cfg.CodexCLI, os.Args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Stdin, cmd.Stdout = os.Stdin, os.Stdout
	var tap *stderrTap
	if cfg.Enabled && isAppServer(os.Args[1:]) {
		tap = newStderrTap(cfg)
		cmd.Stderr = tap
	} else {
		cmd.Stderr = os.Stderr
	}
	// A job closes if the desktop terminates this wrapper, avoiding an orphan
	// CLI process. Capture failures never turn this into a prerequisite for Codex.
	job := newChildJob()
	if job != 0 {
		defer windows.CloseHandle(job)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "Codex timing capture:", err)
		return 1
	}
	if job != 0 {
		process, openErr := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
		if openErr == nil {
			_ = windows.AssignProcessToJobObject(job, process)
			windows.CloseHandle(process)
		}
	}
	if tap != nil {
		tap.started(cmd.Process.Pid)
	}
	err = cmd.Wait()
	if tap != nil {
		tap.finish(100 * time.Millisecond)
	}
	if err == nil {
		return 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode()
	}
	fmt.Fprintln(os.Stderr, "Codex timing capture:", err)
	return 1
}

func isAppServer(args []string) bool {
	for _, arg := range args {
		if arg == "app-server" {
			return true
		}
	}
	return false
}

func newChildJob() windows.Handle {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	// Only the CLI belongs to this extra cleanup job. Tools and applications it
	// starts silently break away, preserving their original host-job/lifetime
	// behavior (closing Codex must not newly kill an editor or background tool).
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if err != nil {
		windows.CloseHandle(job)
		return 0
	}
	return job
}
