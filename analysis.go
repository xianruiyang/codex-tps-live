package codextps

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var sessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ResponseSample is one measurable model sampling cycle.
type ResponseSample struct {
	SessionID, TurnID                          string
	ResponseIndex, TurnResponseIndex           int
	Model, ReasoningEffort                     string
	StartedAt, CompletedAt, UsageRecordedAt    time.Time
	ElapsedSeconds                             float64
	InputTokens, CachedInputTokens             int64
	CacheWriteInputTokens, OutputTokens        int64
	ReasoningOutputTokens, VisibleOutputTokens int64
	TotalTokens                                int64
}

func (s ResponseSample) EffectiveTPS() float64 { return float64(s.OutputTokens) / s.ElapsedSeconds }
func (s ResponseSample) VisibleTPS() float64 {
	return float64(s.VisibleOutputTokens) / s.ElapsedSeconds
}
func (s ResponseSample) ReasoningTPS() float64 {
	return float64(s.ReasoningOutputTokens) / s.ElapsedSeconds
}

// AnalysisResult contains all reportable responses from one rollout.
type AnalysisResult struct {
	SessionID, CodexHome, RolloutPath string
	Samples                           []ResponseSample
	Warnings                          []string
}

func resolveCodexHome(explicit string) (string, error) {
	value := explicit
	if value == "" {
		value = os.Getenv("CODEX_HOME")
	}
	if value == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		value = filepath.Join(home, ".codex")
	}
	return absoluteExpanded(value)
}

func absoluteExpanded(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, path[2:])
		}
	}
	return filepath.Abs(path)
}

func findRollout(codexHome, sessionID string) (string, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return "", fmt.Errorf("invalid Codex session ID: %q", sessionID)
	}
	info, err := os.Stat(codexHome)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("Codex home does not exist: %s", codexHome)
	}
	matches := map[string]struct{}{}
	for _, directory := range []string{"sessions", "archived_sessions"} {
		root := filepath.Join(codexHome, directory)
		if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
			continue
		}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") && strings.Contains(entry.Name(), sessionID) {
				absolute, err := filepath.Abs(path)
				if err != nil {
					return err
				}
				matches[absolute] = struct{}{}
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	paths := make([]string, 0, len(matches))
	for path := range matches {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return "", fmt.Errorf("session %s was not found under %s", sessionID, codexHome)
	}
	if len(paths) > 1 {
		var rendered strings.Builder
		for _, path := range paths {
			fmt.Fprintf(&rendered, "\n  - %s", path)
		}
		return "", fmt.Errorf("session %s matched more than one rollout:%s", sessionID, rendered.String())
	}
	return paths[0], nil
}

func analyzeRollout(rolloutPath, expectedSessionID, codexHome string) (AnalysisResult, error) {
	result := AnalysisResult{SessionID: expectedSessionID, CodexHome: codexHome, RolloutPath: rolloutPath}
	file, err := os.Open(rolloutPath)
	if err != nil {
		return result, fmt.Errorf("could not open rollout %s: %w", rolloutPath, err)
	}
	defer file.Close()

	var turnID, model, effort string
	var samplingStart, generatedEnd *time.Time
	turnResponseIndex := 0
	sawTaskStarted := false
	reader := bufio.NewReaderSize(file, 128*1024)
	for lineNumber := 1; ; lineNumber++ {
		line, readErr := reader.ReadString('\n')
		if len(strings.TrimSpace(line)) != 0 {
			var decoded any
			decoder := json.NewDecoder(strings.NewReader(line))
			decoder.UseNumber()
			if err := decoder.Decode(&decoded); err != nil {
				if errors.Is(readErr, io.EOF) && !strings.HasSuffix(line, "\n") {
					result.Warnings = append(result.Warnings, fmt.Sprintf("line %d: ignored an incomplete final JSONL record", lineNumber))
					break
				}
				return result, fmt.Errorf("invalid JSON in %s at line %d: %v", rolloutPath, lineNumber, err)
			}
			row, ok := decoded.(map[string]any)
			if !ok {
				return result, fmt.Errorf("invalid JSONL object in %s at line %d", rolloutPath, lineNumber)
			}
			timestampText, ok := row["timestamp"].(string)
			if ok {
				timestamp, parseErr := time.Parse(time.RFC3339Nano, timestampText)
				if parseErr != nil {
					result.Warnings = append(result.Warnings, fmt.Sprintf("line %d: skipped invalid timestamp", lineNumber))
				} else {
					recordType, _ := row["type"].(string)
					payload, _ := row["payload"].(map[string]any)
					if payload == nil {
						payload = map[string]any{}
					}
					payloadType, _ := payload["type"].(string)
					switch {
					case recordType == "session_meta":
						recordedID, _ := payload["id"].(string)
						if recordedID == "" {
							recordedID, _ = payload["session_id"].(string)
						}
						if recordedID != "" && recordedID != expectedSessionID {
							return result, fmt.Errorf("rollout session ID does not match the requested session: %s != %s", recordedID, expectedSessionID)
						}
						if recordedID != "" {
							result.SessionID = recordedID
						}
					case recordType == "turn_context":
						if value, ok := payload["turn_id"].(string); ok {
							turnID = value
						}
						if value, ok := payload["model"].(string); ok {
							model = value
						}
						if value, ok := payload["effort"].(string); ok {
							effort = value
						}
						if generatedEnd == nil {
							samplingStart = timePointer(timestamp)
						}
					case recordType == "event_msg" && payloadType == "task_started":
						sawTaskStarted = true
						if value, ok := payload["turn_id"].(string); ok {
							turnID = value
						}
						samplingStart, generatedEnd = timePointer(timestamp), nil
						turnResponseIndex = 0
					case recordType == "event_msg" && payloadType == "user_message":
						if generatedEnd == nil {
							samplingStart = timePointer(timestamp)
						}
					case recordType == "response_item":
						if isGeneratedItem(payload) {
							generatedEnd = timePointer(timestamp)
						} else if generatedEnd == nil && isInputItem(payload) {
							samplingStart = timePointer(timestamp)
						}
					case recordType == "event_msg" && payloadType == "token_count":
						info, _ := payload["info"].(map[string]any)
						usage, _ := info["last_token_usage"].(map[string]any)
						if info == nil {
							result.Warnings = append(result.Warnings, fmt.Sprintf("line %d: skipped token count without usage", lineNumber))
						} else if usage == nil {
							result.Warnings = append(result.Warnings, fmt.Sprintf("line %d: skipped token count without last usage", lineNumber))
						} else if samplingStart == nil || generatedEnd == nil {
							result.Warnings = append(result.Warnings, fmt.Sprintf("line %d: skipped usage without a measurable response window", lineNumber))
						} else {
							elapsed := generatedEnd.Sub(*samplingStart).Seconds()
							output, reasoning := integer(usage, "output_tokens"), integer(usage, "reasoning_output_tokens")
							if elapsed <= 0 {
								result.Warnings = append(result.Warnings, fmt.Sprintf("line %d: skipped non-positive response duration", lineNumber))
							} else if output <= 0 {
								result.Warnings = append(result.Warnings, fmt.Sprintf("line %d: skipped response without output tokens", lineNumber))
							} else {
								visible := output - reasoning
								if visible < 0 {
									visible = 0
									result.Warnings = append(result.Warnings, fmt.Sprintf("line %d: reasoning tokens exceeded output tokens; visible tokens were clamped to zero", lineNumber))
								}
								turnResponseIndex++
								result.Samples = append(result.Samples, ResponseSample{SessionID: result.SessionID, TurnID: turnID, ResponseIndex: len(result.Samples) + 1, TurnResponseIndex: turnResponseIndex, Model: model, ReasoningEffort: effort, StartedAt: *samplingStart, CompletedAt: *generatedEnd, UsageRecordedAt: timestamp, ElapsedSeconds: elapsed, InputTokens: integer(usage, "input_tokens"), CachedInputTokens: integer(usage, "cached_input_tokens"), CacheWriteInputTokens: integer(usage, "cache_write_input_tokens"), OutputTokens: output, ReasoningOutputTokens: reasoning, VisibleOutputTokens: visible, TotalTokens: integer(usage, "total_tokens")})
							}
						}
						samplingStart, generatedEnd = timePointer(timestamp), nil
					}
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return result, readErr
		}
	}
	if !sawTaskStarted {
		return result, fmt.Errorf("this legacy rollout predates reliable per-response timing; codex-tps cannot calculate trustworthy TPS for it")
	}
	if len(result.Samples) == 0 {
		return result, fmt.Errorf("the rollout did not contain any measurable model responses with token usage")
	}
	return result, nil
}

func timePointer(value time.Time) *time.Time { copy := value; return &copy }
func integer(mapping map[string]any, key string) int64 {
	value, ok := mapping[key].(json.Number)
	if !ok {
		return 0
	}
	integer, err := value.Int64()
	if err != nil {
		return 0
	}
	return integer
}
func isGeneratedItem(payload map[string]any) bool {
	typeName, _ := payload["type"].(string)
	if typeName == "reasoning" {
		return true
	}
	if role, _ := payload["role"].(string); typeName == "message" && role == "assistant" {
		return true
	}
	return strings.HasSuffix(typeName, "_call")
}
func isInputItem(payload map[string]any) bool {
	typeName, _ := payload["type"].(string)
	if typeName == "function_call_output" || typeName == "custom_tool_call_output" || typeName == "agent_message" {
		return true
	}
	role, _ := payload["role"].(string)
	return typeName == "message" && (role == "user" || role == "developer" || role == "system")
}
