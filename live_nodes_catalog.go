package codextps

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type liveNode struct {
	ID           string `json:"id"`
	Subscription string `json:"subscription"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	Source       string `json:"source"`
	Issue        string `json:"issue,omitempty"`
}

type liveNodeSubscription struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Kind   string     `json:"kind"`
	Active bool       `json:"active"`
	Note   string     `json:"note,omitempty"`
	Nodes  []liveNode `json:"nodes"`
}

type liveNodeCatalog struct {
	Subscriptions []liveNodeSubscription `json:"subscriptions"`
	Ready         bool                   `json:"ready"`
	Error         string                 `json:"error,omitempty"`
	Engine        string                 `json:"engine,omitempty"`
	CurlAvailable bool                   `json:"curl_available"`
	ClashURL      string                 `json:"clash_url"`
	UnifiedDelay  bool                   `json:"unified_delay"`
	Warning       string                 `json:"warning,omitempty"`
	Updated       time.Time              `json:"updated"`
}

type liveNodeProfile struct {
	Proxies   []map[string]any          `yaml:"proxies"`
	Groups    []map[string]any          `yaml:"proxy-groups"`
	Providers map[string]map[string]any `yaml:"proxy-providers"`
}

type liveNodeSpec struct {
	public liveNode
	raw    map[string]any
	peers  map[string]*liveNodeSpec
}

type liveNodeInventory struct {
	catalog liveNodeCatalog
	specs   map[string]*liveNodeSpec
	binary  string
	curl    string
}

func liveNodeID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:12])
}

func liveNodeText(value any) string {
	text, _ := value.(string)
	return text
}

// Decode YAML structurally: proxy-groups and enhancement profiles are not nodes.
// Raw documents stay on the backend; neither passwords nor subscription URLs
// are included in any catalog or test result.
func readLiveNodeYAML(path string, out any) error {
	info, err := os.Stat(path)
	if err != nil || info.Size() > 32<<20 {
		return errors.New("本地配置不存在、不可读或超过 32 MiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.New("无法读取本地配置")
	}
	if yaml.Unmarshal(data, out) != nil {
		return errors.New("本地 YAML 格式无法解析")
	}
	return nil
}

func discoverLiveNodes(homeOverride, binaryOverride string) liveNodeInventory {
	inv := liveNodeInventory{
		catalog: liveNodeCatalog{Subscriptions: []liveNodeSubscription{}, Updated: time.Now()},
		specs:   make(map[string]*liveNodeSpec),
	}
	home := homeOverride
	if home == "" {
		base, _ := os.UserConfigDir()
		for _, id := range []string{"io.github.clash-verge-rev.clash-verge-rev", "io.github.clash-verge.clash-verge"} {
			candidate := filepath.Join(base, id)
			if _, err := os.Stat(filepath.Join(candidate, "profiles.yaml")); err == nil {
				home = candidate
				break
			}
		}
	}
	if home == "" {
		inv.catalog.Error = "没有发现 Clash Verge 订阅目录。请先安装并导入订阅；自定义目录可通过 --clash-home 指定。"
		return inv
	}
	inv.catalog.ClashURL = "http://cp.cloudflare.com/generate_204"
	var verge struct {
		LatencyURL string `yaml:"default_latency_test"`
	}
	if readLiveNodeYAML(filepath.Join(home, "verge.yaml"), &verge) == nil && strings.TrimSpace(verge.LatencyURL) != "" {
		if validLiveClashURL(strings.TrimSpace(verge.LatencyURL)) {
			inv.catalog.ClashURL = strings.TrimSpace(verge.LatencyURL)
		} else {
			inv.catalog.Warning = "Clash 自定义测速地址无法安全导入，已使用 GUI 默认公开地址。"
		}
	}
	var runtimeConfig struct {
		UnifiedDelay bool `yaml:"unified-delay"`
	}
	if readLiveNodeYAML(filepath.Join(home, "clash-verge.yaml"), &runtimeConfig) == nil {
		inv.catalog.UnifiedDelay = runtimeConfig.UnifiedDelay
	}
	var metadata struct {
		Current yaml.Node `yaml:"current"`
		Items   []struct {
			UID  string `yaml:"uid"`
			Kind string `yaml:"type"`
			Name string `yaml:"name"`
			File string `yaml:"file"`
		} `yaml:"items"`
	}
	if err := readLiveNodeYAML(filepath.Join(home, "profiles.yaml"), &metadata); err != nil {
		inv.catalog.Error = "无法读取 Clash Verge 订阅清单：" + err.Error()
		return inv
	}
	active := map[string]bool{}
	if metadata.Current.Kind == yaml.ScalarNode {
		active[metadata.Current.Value] = true
	} else if metadata.Current.Kind == yaml.SequenceNode {
		for _, entry := range metadata.Current.Content {
			active[entry.Value] = true
		}
	}
	for _, item := range metadata.Items {
		if item.Kind != "remote" && item.Kind != "local" {
			continue
		}
		sub := liveNodeSubscription{ID: liveNodeID(item.UID, item.File), Name: item.Name, Kind: item.Kind,
			Active: active[item.UID], Nodes: []liveNode{}}
		if sub.Name == "" {
			sub.Name = "未命名订阅"
		}
		file := item.File
		if file == "" {
			file = item.UID + ".yaml"
		}
		if !filepath.IsLocal(file) {
			sub.Note = "订阅文件路径不在本地 profiles 目录内，未读取。"
			inv.catalog.Subscriptions = append(inv.catalog.Subscriptions, sub)
			continue
		}
		var doc liveNodeProfile
		if err := readLiveNodeYAML(filepath.Join(home, "profiles", file), &doc); err != nil {
			sub.Note = err.Error()
			inv.catalog.Subscriptions = append(inv.catalog.Subscriptions, sub)
			continue
		}
		sources := map[string]string{}
		// Only existing local provider caches are read. Never fetch a provider URL.
		for providerName, provider := range doc.Providers {
			path := liveNodeText(provider["path"])
			if path == "" {
				sub.Note += "外部 provider 没有本地缓存路径，未联网拉取。 "
				continue
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(home, path)
			}
			var cached liveNodeProfile
			if readLiveNodeYAML(path, &cached) != nil {
				sub.Note += "部分 provider 本地缓存不可读。 "
				continue
			}
			for _, raw := range cached.Proxies {
				doc.Proxies = append(doc.Proxies, raw)
				sources[liveNodeText(raw["name"])] = "本地 provider 缓存 · " + providerName
			}
		}
		// For the active subscription, use already-generated effective definitions
		// where available. Do not execute enhancement JavaScript ourselves.
		if sub.Active && len(active) == 1 {
			var effective liveNodeProfile
			if readLiveNodeYAML(filepath.Join(home, "clash-verge.yaml"), &effective) == nil {
				byName := map[string]map[string]any{}
				for _, raw := range effective.Proxies {
					byName[liveNodeText(raw["name"])] = raw
				}
				seen := map[string]bool{}
				for i, raw := range doc.Proxies {
					name := liveNodeText(raw["name"])
					seen[name] = true
					if updated, ok := byName[name]; ok {
						doc.Proxies[i] = updated
						sources[name] = "当前生效配置"
					}
				}
				for _, raw := range effective.Proxies {
					name := liveNodeText(raw["name"])
					if !seen[name] {
						doc.Proxies = append(doc.Proxies, raw)
						sources[name] = "当前生效配置 · 覆写新增"
						seen[name] = true
					}
				}
			}
		}
		peers := make(map[string]*liveNodeSpec)
		counts := map[string]int{}
		for _, raw := range doc.Proxies {
			counts[liveNodeText(raw["name"])]++
		}
		for index, raw := range doc.Proxies {
			name := liveNodeText(raw["name"])
			node := liveNode{ID: liveNodeID(sub.ID, name, fmt.Sprint(index)), Subscription: sub.ID,
				Name: name, Type: liveNodeText(raw["type"]), Source: sources[name]}
			if node.Source == "" {
				node.Source = "本地订阅原始定义"
			}
			if name == "" || node.Type == "" {
				node.Issue = "节点缺少名称或协议类型"
			}
			if counts[name] > 1 {
				node.Issue = "同一订阅内有重名节点，无法可靠匹配依赖"
			}
			spec := &liveNodeSpec{public: node, raw: raw, peers: peers}
			peers[name], inv.specs[node.ID] = spec, spec
			sub.Nodes = append(sub.Nodes, node)
		}
		inv.catalog.Subscriptions = append(inv.catalog.Subscriptions, sub)
	}
	inv.binary = binaryOverride
	if inv.binary == "" {
		inv.binary, _ = findLiveNodeEngine()
	}
	inv.curl, _ = exec.LookPath("curl.exe")
	inv.catalog.CurlAvailable = inv.curl != ""
	if inv.binary == "" {
		inv.catalog.Error = "已读取订阅，但没有找到 Mihomo。请打开 Clash Verge，或通过 --mihomo-path 指定内核。"
	} else if _, err := os.Stat(inv.binary); err != nil {
		inv.catalog.Error = "指定的 Mihomo 内核不存在。"
	} else {
		inv.catalog.Ready = true
		inv.catalog.Engine = filepath.Base(inv.binary)
		if inv.curl == "" {
			inv.catalog.Warning += " 没有找到 curl.exe，目前只能进行普通 Clash 测速。"
		}
	}
	return inv
}

func validLiveClashURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil
}

// Rename every copied outbound and its dependencies; identical names from
// different subscriptions must never refer to one another.
func compileLiveNodeSpecs(selected []*liveNodeSpec) ([]map[string]any, map[string]string, error) {
	var compiled []map[string]any
	aliases := map[string]string{}
	visiting := map[string]bool{}
	var add func(*liveNodeSpec) (string, error)
	add = func(spec *liveNodeSpec) (string, error) {
		if visiting[spec.public.ID] {
			return "", errors.New("节点代理链存在循环依赖")
		}
		if alias, ok := aliases[spec.public.ID]; ok {
			return alias, nil
		}
		if spec.public.Issue != "" {
			return "", errors.New(spec.public.Issue)
		}
		visiting[spec.public.ID] = true
		defer delete(visiting, spec.public.ID)
		data, _ := yaml.Marshal(spec.raw)
		var raw map[string]any
		if yaml.Unmarshal(data, &raw) != nil {
			return "", errors.New("无法复制节点定义")
		}
		alias := "probe_" + spec.public.ID
		raw["name"] = alias
		if dependency := liveNodeText(raw["dialer-proxy"]); dependency != "" && dependency != "DIRECT" {
			peer := spec.peers[dependency]
			if peer == nil {
				return "", errors.New("节点依赖代理组或订阅外节点，不能确定独立测试路线：" + dependency)
			}
			renamed, err := add(peer)
			if err != nil {
				return "", err
			}
			raw["dialer-proxy"] = renamed
		}
		for _, field := range []string{"certificate", "private-key"} {
			if value := liveNodeText(raw[field]); value != "" && !strings.Contains(value, "-----BEGIN") {
				return "", errors.New("节点依赖外部证书文件，暂不支持隔离复制")
			}
		}
		aliases[spec.public.ID] = alias
		compiled = append(compiled, raw)
		return alias, nil
	}
	for _, spec := range selected {
		if _, err := add(spec); err != nil {
			return nil, nil, fmt.Errorf("%s：%s", spec.public.Name, err)
		}
	}
	return compiled, aliases, nil
}
