//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"
)

// Server diagnostics only. None of these values represents measured client TTFT.
type capturedTiming struct {
	EventID            string             `json:"event_id"`
	At                 time.Time          `json:"at"`
	RequestStartMS     int64              `json:"request_start_ms"`
	ThreadID           string             `json:"thread_id"`
	TurnID             string             `json:"turn_id"`
	Model              string             `json:"model"`
	ResponseID         string             `json:"response_id,omitempty"`
	PreviousResponseID string             `json:"previous_response_id,omitempty"`
	Warmup             bool               `json:"warmup"`
	ConnectionReused   bool               `json:"connection_reused"`
	Shape              string             `json:"shape"`
	Scope              string             `json:"scope,omitempty"`
	CriticalScope      string             `json:"critical_scope,omitempty"`
	Coverage           string             `json:"coverage,omitempty"`
	Boundary           string             `json:"boundary_type,omitempty"`
	Metrics            map[string]float64 `json:"timing_metrics,omitempty"`
	CriticalPath       map[string]float64 `json:"critical_path,omitempty"`
}

// The second result distinguishes unrelated stderr, invalid JSON, unknown
// schema and supported diagnostics. Unknown valid payloads still yield a safe
// record instead of silently disappearing. No arbitrary payload is persisted.
func parseCapturedTiming(line []byte) (capturedTiming, string) {
	var record capturedTiming
	if !bytes.Contains(line, []byte(captureTarget)) {
		return record, ""
	}
	var event struct {
		Timestamp string                     `json:"timestamp"`
		Target    string                     `json:"target"`
		Fields    map[string]json.RawMessage `json:"fields"`
	}
	if json.Unmarshal(line, &event) != nil {
		return record, "malformed_event"
	}
	if event.Target != captureTarget {
		return record, ""
	}
	if event.Fields == nil {
		_ = json.Unmarshal(line, &event.Fields)
	}
	var payload map[string]json.RawMessage
	raw := event.Fields["payload"]
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return record, "malformed_payload"
		}
		raw = []byte(text)
	}
	if json.Unmarshal(raw, &payload) != nil || payload == nil {
		return record, "malformed_payload"
	}
	text := func(m map[string]json.RawMessage, key string, limit int) string {
		var value string
		if json.Unmarshal(m[key], &value) != nil || len(value) > limit {
			return ""
		}
		// IDs/model names and schema enums only, never free-form values.
		for _, c := range value {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
				return ""
			}
		}
		return value
	}
	numbers := func(m map[string]json.RawMessage, keys []string) map[string]float64 {
		out := map[string]float64{}
		for _, key := range keys {
			raw := m[key]
			var value float64
			if len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && json.Unmarshal(raw, &value) == nil && value >= 0 {
				out[key] = value
			}
		}
		return out
	}
	var metrics map[string]json.RawMessage
	_ = json.Unmarshal(payload["timing_metrics"], &metrics)
	if metrics == nil {
		metrics = payload
	}
	var critical map[string]json.RawMessage
	_ = json.Unmarshal(metrics["critical_path"], &critical)
	if critical == nil {
		_ = json.Unmarshal(payload["critical_path"], &critical)
	}
	record.Metrics = numbers(metrics, []string{"responses_duration_excl_engine_and_client_tool_time_ms", "engine_service_total_ms", "engine_iapi_ttft_total_ms", "engine_service_ttft_total_ms", "engine_iapi_tbt_across_engine_calls_ms", "engine_service_tbt_across_engine_calls_ms"})
	record.CriticalPath = numbers(critical, []string{"responses_harness_unblocked_ms", "responses_pre_inference_ms", "engine_wall_ms", "client_tool_pause_ms", "responses_other_ms"})
	record.Scope = text(metrics, "scope", 64)
	if record.Scope == "" {
		record.Scope = text(payload, "scope", 64)
	}
	record.CriticalScope = text(critical, "scope", 64)
	record.Coverage = text(critical, "coverage", 64)
	record.Boundary = text(critical, "boundary_type", 64)
	record.Shape = "schema_unrecognized"
	if len(record.CriticalPath) > 0 {
		record.Shape = "critical_path"
	} else if len(record.Metrics) > 0 {
		record.Shape = "server_engine_timing"
	}
	record.At, _ = time.Parse(time.RFC3339Nano, event.Timestamp)
	start := text(event.Fields, "request_start_ms", 24)
	if start == "" {
		start = string(event.Fields["request_start_ms"])
	}
	record.RequestStartMS, _ = strconv.ParseInt(start, 10, 64)
	record.ThreadID = text(event.Fields, "thread_id", 64)
	record.TurnID = text(event.Fields, "turn_id", 64)
	record.Model = text(event.Fields, "model", 128)
	record.PreviousResponseID = text(event.Fields, "previous_response_id", 256)
	record.ResponseID = text(payload, "response_id", 256)
	_ = json.Unmarshal(event.Fields["warmup"], &record.Warmup)
	_ = json.Unmarshal(event.Fields["connection_reused"], &record.ConnectionReused)
	return record, record.Shape
}
