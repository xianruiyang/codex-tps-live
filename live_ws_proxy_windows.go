//go:build windows

package codextps

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
)

func liveWSProxyPipeController(configured, secret string) (liveWSProxyController, bool) {
	const prefix = `\\.\pipe\`
	if !strings.HasPrefix(strings.ToLower(configured), prefix) {
		return liveWSProxyController{}, false
	}
	name := configured[len(prefix):]
	candidates := []string{configured}
	const sidecar = "verge-mihomo-sidecar-release-"
	if strings.HasPrefix(name, sidecar) {
		owner := strings.TrimPrefix(name, sidecar)
		if owner != "" && !strings.ContainsAny(owner, `\/`) {
			candidates = []string{prefix + "verge-mihomo-production-" + owner, configured}
		}
	}
	for _, pipe := range candidates {
		// Enumerate no namespaces: probe only the configured pipe and its same-owner
		// official service-mode counterpart; never enumerate other users' pipes.
		probeCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		probe, err := winio.DialPipeContext(probeCtx, pipe)
		cancel()
		if err != nil {
			continue
		}
		probe.Close()
		return liveWSProxyController{address: "localhost", secret: secret, source: "Mihomo本机IPC", dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			child, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			return winio.DialPipeContext(child, pipe)
		}}, true
	}
	return liveWSProxyController{}, false
}
