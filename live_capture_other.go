//go:build !windows

package codextps

func captureProcessAlive(pid int) bool { return false }
