package codextps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSessionID = "019ffb1f-f334-7e91-9a25-08d5e9e40d42"
const testTurnID = "019ffb20-1111-7111-8111-111111111111"

func record(timestamp, recordType string, payload map[string]any) map[string]any {
	return map[string]any{"timestamp": timestamp, "type": recordType, "payload": payload}
}
func tokenCount(timestamp string, output, reasoning int) map[string]any {
	usage := map[string]any{"input_tokens": 1000, "cached_input_tokens": 800, "cache_write_input_tokens": 100, "output_tokens": output, "reasoning_output_tokens": reasoning, "total_tokens": 1000 + output}
	return record(timestamp, "event_msg", map[string]any{"type": "token_count", "info": map[string]any{"last_token_usage": usage, "total_token_usage": usage}})
}
func sampleRecords() []map[string]any {
	return []map[string]any{
		record("2026-08-13T12:00:00.000Z", "session_meta", map[string]any{"id": testSessionID, "originator": "Codex Desktop"}),
		record("2026-08-13T12:00:00.100Z", "event_msg", map[string]any{"type": "task_started", "turn_id": testTurnID}),
		record("2026-08-13T12:00:00.200Z", "turn_context", map[string]any{"turn_id": testTurnID, "model": "gpt-test", "effort": "high"}),
		record("2026-08-13T12:00:01.000Z", "response_item", map[string]any{"type": "message", "role": "user", "content": "private prompt"}),
		record("2026-08-13T12:00:03.000Z", "response_item", map[string]any{"type": "reasoning", "summary": []any{}}),
		record("2026-08-13T12:00:05.000Z", "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "call-1"}),
		record("2026-08-13T12:00:15.000Z", "response_item", map[string]any{"type": "custom_tool_call_output", "call_id": "call-1"}),
		tokenCount("2026-08-13T12:00:15.001Z", 100, 40),
		record("2026-08-13T12:00:17.001Z", "response_item", map[string]any{"type": "reasoning", "summary": []any{}}),
		record("2026-08-13T12:00:19.001Z", "response_item", map[string]any{"type": "message", "role": "assistant", "content": []any{}}),
		tokenCount("2026-08-13T12:00:19.002Z", 80, 20),
		record("2026-08-13T12:00:19.010Z", "event_msg", map[string]any{"type": "task_complete", "turn_id": testTurnID}),
	}
}
func writeRollout(t *testing.T, path string, records []map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, item := range records {
		if err := encoder.Encode(item); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAnalyzeResponsesExcludesToolRuntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-"+testSessionID+".jsonl")
	writeRollout(t, path, sampleRecords())
	result, err := analyzeRollout(path, testSessionID, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Samples) != 2 {
		t.Fatalf("samples=%d", len(result.Samples))
	}
	first, second := result.Samples[0], result.Samples[1]
	if first.ElapsedSeconds != 4 || first.EffectiveTPS() != 25 || first.ReasoningTPS() != 10 || first.VisibleTPS() != 15 {
		t.Fatalf("first=%+v", first)
	}
	if first.Model != "gpt-test" || first.ReasoningEffort != "high" || second.ElapsedSeconds != 4 || second.EffectiveTPS() != 20 || second.TurnResponseIndex != 2 {
		t.Fatalf("samples=%+v", result.Samples)
	}
}
func TestFindActiveArchivedMissingAndDuplicate(t *testing.T) {
	home := t.TempDir()
	active := filepath.Join(home, "sessions", "2026", "08", "13", "rollout-"+testSessionID+".jsonl")
	writeRollout(t, active, sampleRecords())
	found, err := findRollout(home, testSessionID)
	if err != nil || found != active {
		t.Fatalf("found=%q err=%v", found, err)
	}
	if err := os.Remove(active); err != nil {
		t.Fatal(err)
	}
	archived := filepath.Join(home, "archived_sessions", "rollout-"+testSessionID+".jsonl")
	writeRollout(t, archived, sampleRecords())
	found, err = findRollout(home, testSessionID)
	if err != nil || found != archived {
		t.Fatalf("found=%q err=%v", found, err)
	}
	writeRollout(t, active, sampleRecords())
	if _, err = findRollout(home, testSessionID); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("err=%v", err)
	}
	if _, err = findRollout(home, "not-a-session"); err == nil || !strings.Contains(err.Error(), "invalid Codex session ID") {
		t.Fatalf("err=%v", err)
	}
}
func TestIncompleteFinalRecordAndLegacyRejection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-"+testSessionID+".jsonl")
	writeRollout(t, path, sampleRecords())
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString(`{"timestamp":"unfinished"`)
	_ = file.Close()
	result, err := analyzeRollout(path, testSessionID, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) == 0 || !strings.Contains(result.Warnings[len(result.Warnings)-1], "incomplete final") {
		t.Fatalf("warnings=%v", result.Warnings)
	}
	records := sampleRecords()
	filtered := records[:0]
	for _, item := range records {
		payload, _ := item["payload"].(map[string]any)
		if payload["type"] != "task_started" {
			filtered = append(filtered, item)
		}
	}
	writeRollout(t, path, filtered)
	if _, err = analyzeRollout(path, testSessionID, filepath.Dir(path)); err == nil || !strings.Contains(err.Error(), "legacy rollout") {
		t.Fatalf("err=%v", err)
	}
}
func TestCompleteMalformedJSONRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-"+testSessionID+".jsonl")
	if err := os.WriteFile(path, []byte("{\"bad\": }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := analyzeRollout(path, testSessionID, filepath.Dir(path)); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("err=%v", err)
	}
}
