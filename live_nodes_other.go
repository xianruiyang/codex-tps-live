//go:build !windows

package codextps

import (
	"errors"
	"os/exec"
)

func lockLiveNodePlans(path string) (func(), error) {
	return nil, errors.New("计划测速目前支持 Windows")
}

func hideLiveNodeCommand(cmd *exec.Cmd)                {}
func ownLiveNodeProcess(cmd *exec.Cmd) (func(), error) { return func() {}, nil }
func findLiveNodeEngine() (string, error)              { return exec.LookPath("mihomo") }
