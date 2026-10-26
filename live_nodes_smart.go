package codextps

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// The dispatcher owns all exclusion state. Each round finishes before the next
// is queued, and all selected Clash rounds finish before any Codex stage starts.
// This prevents in-flight speculative requests from bypassing an exclusion.
func (s *liveNodeService) runSmartNodes(ctx context.Context, selected []*liveNodeSpec, job *liveNodeJob,
	probe func(int, string, int) liveNodeResult, record func(liveNodeResult)) {
	selectedTests := map[string]bool{}
	for _, test := range job.Options.Tests {
		selectedTests[test] = true
	}
	allBlocked := map[string]liveNodeExclusion{}
	endpointBlocked := map[string]liveNodeExclusion{}
	completed := liveNodeCompleted(job)
	for _, block := range job.Exclusions {
		if block.Scope == "all" {
			allBlocked[block.Node] = block
		} else {
			endpointBlocked[block.Node+"/"+block.Scope] = block
		}
	}
	labels := map[string]string{"clash": "Clash", "http": "HTTP", "websocket": "WebSocket"}
	for _, endpoint := range []string{"clash", "http", "websocket"} {
		if !selectedTests[endpoint] {
			continue
		}
		for round := 1; round <= job.Options.Rounds; round++ {
			if ctx.Err() != nil {
				return
			}
			s.mu.Lock()
			if job.Status != "stopping" {
				job.Message = fmt.Sprintf("智能测速：%s 阶段 · 第 %d/%d 轮", labels[endpoint], round, job.Options.Rounds)
			}
			s.mu.Unlock()
			var candidates []int
			for index, spec := range selected {
				if ctx.Err() != nil {
					return
				}
				if completed[liveNodeResultKey(spec.public.ID, endpoint, round)] {
					continue
				}
				block, excluded := allBlocked[spec.public.ID]
				if !excluded {
					block, excluded = endpointBlocked[spec.public.ID+"/"+endpoint]
				}
				if excluded {
					record(liveNodeResult{Node: spec.public.ID, Endpoint: endpoint, Round: round, Started: time.Now(), Outcome: "skipped", Exit: -1, Note: block.Message})
				} else {
					candidates = append(candidates, index)
				}
			}
			if len(candidates) == 0 {
				continue
			}
			tasks := make(chan int, len(candidates))
			for _, index := range candidates {
				tasks <- index
			}
			close(tasks)
			results := make(chan liveNodeResult, min(job.Options.Workers, len(candidates)))
			var workers sync.WaitGroup
			for range min(job.Options.Workers, len(candidates)) {
				workers.Add(1)
				go func() {
					defer workers.Done()
					for index := range tasks {
						if ctx.Err() != nil {
							return
						}
						row := probe(index, endpoint, round)
						if ctx.Err() != nil {
							return
						}
						select {
						case results <- row:
						case <-ctx.Done():
							return
						}
					}
				}()
			}
			go func() { workers.Wait(); close(results) }()
			for row := range results {
				if ctx.Err() != nil {
					continue
				}
				block := liveNodeSmartExclusion(job.Options, row)
				if block != nil {
					row.Note = block.Message
					if block.Scope == "all" {
						allBlocked[row.Node] = *block
					} else {
						endpointBlocked[row.Node+"/"+endpoint] = *block
					}
					s.mu.Lock()
					job.Exclusions = append(job.Exclusions, *block)
					s.mu.Unlock()
				}
				record(row)
			}
		}
	}
}

func liveNodeSmartExclusion(options liveNodeOptions, row liveNodeResult) *liveNodeExclusion {
	if row.Outcome == "skipped" {
		return nil
	}
	if row.Endpoint == "clash" {
		if row.Outcome != "clash_ok" || row.Delay == nil {
			return &liveNodeExclusion{Node: row.Node, Scope: "all", Reason: "clash_failed", Message: "Clash 测速失败，已排除整节点的全部后续测试"}
		}
		if *row.Delay > float64(options.ClashMaxDelayMS) {
			return &liveNodeExclusion{Node: row.Node, Scope: "all", Reason: "clash_slow", Message: fmt.Sprintf("Clash 延迟 %.0f ms 超过 %d ms，已排除整节点", *row.Delay, options.ClashMaxDelayMS)}
		}
	} else if row.Outcome != "auth_reached" && row.Outcome != "upgrade_accepted" {
		label := map[string]string{"http": "HTTP", "websocket": "WebSocket"}[row.Endpoint]
		return &liveNodeExclusion{Node: row.Node, Scope: row.Endpoint, Reason: row.Endpoint + "_failed", Message: label + " 失败，仅停止该项目的后续轮次"}
	}
	return nil
}
