//go:build !windows

package codextps

func captureProcessAlive(pid int, created int64) bool { return false }

func captureBackendAlive(pid int, created int64) bool { return false }
