//go:build !windows

package codextps

import "net"

type liveTrafficOwner struct {
	Process string
	PID     uint32
}

func liveTrafficSocketKey(ip, port string) string { return net.JoinHostPort(ip, port) }
func liveTrafficOwners([]liveTrafficWireConnection) (map[string]liveTrafficOwner, string) {
	return map[string]liveTrafficOwner{}, "当前平台仅使用代理进程字段"
}
