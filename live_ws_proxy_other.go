//go:build !windows

package codextps

func liveWSProxyPipeController(string, string) (liveWSProxyController, bool) {
	return liveWSProxyController{}, false
}
