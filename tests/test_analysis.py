from __future__ import annotations

import json
from pathlib import Path

import pytest

from codex_tps.analysis import (
    CodexTpsError,
    analyze_rollout,
    find_rollout,
    resolve_codex_home,
)


SESSION_ID = "019ffb1f-f334-7e91-9a25-08d5e9e40d42"
TURN_ID = "019ffb20-1111-7111-8111-111111111111"


def record(timestamp: str, record_type: str, payload: dict) -> dict:
    return {"timestamp": timestamp, "type": record_type, "payload": payload}


def sample_records() -> list[dict]:
    return [
        record(
            "2026-08-13T12:00:00.000Z",
            "session_meta",
            {"id": SESSION_ID, "originator": "Codex Desktop"},
        ),
        record(
            "2026-08-13T12:00:00.100Z",
            "event_msg",
            {"type": "task_started", "turn_id": TURN_ID},
        ),
        record(
            "2026-08-13T12:00:00.200Z",
            "turn_context",
            {"turn_id": TURN_ID, "model": "gpt-test", "effort": "high"},
        ),
        record(
            "2026-08-13T12:00:01.000Z",
            "response_item",
            {"type": "message", "role": "user", "content": "private prompt"},
        ),
        record(
            "2026-08-13T12:00:03.000Z",
            "response_item",
            {"type": "reasoning", "summary": []},
        ),
        record(
            "2026-08-13T12:00:05.000Z",
            "response_item",
            {"type": "custom_tool_call", "name": "exec", "call_id": "call-1"},
        ),
        record(
            "2026-08-13T12:00:15.000Z",
            "response_item",
            {"type": "custom_tool_call_output", "call_id": "call-1"},
        ),
        token_count("2026-08-13T12:00:15.001Z", output=100, reasoning=40),
        record(
            "2026-08-13T12:00:17.001Z",
            "response_item",
            {"type": "reasoning", "summary": []},
        ),
        record(
            "2026-08-13T12:00:19.001Z",
            "response_item",
            {"type": "message", "role": "assistant", "content": []},
        ),
        token_count("2026-08-13T12:00:19.002Z", output=80, reasoning=20),
        record(
            "2026-08-13T12:00:19.010Z",
            "event_msg",
            {"type": "task_complete", "turn_id": TURN_ID},
        ),
    ]


def token_count(timestamp: str, output: int, reasoning: int) -> dict:
    usage = {
        "input_tokens": 1_000,
        "cached_input_tokens": 800,
        "cache_write_input_tokens": 100,
        "output_tokens": output,
        "reasoning_output_tokens": reasoning,
        "total_tokens": 1_000 + output,
    }
    return record(
        timestamp,
        "event_msg",
        {
            "type": "token_count",
            "info": {"last_token_usage": usage, "total_token_usage": usage},
        },
    )


def write_rollout(path: Path, records: list[dict]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        "".join(json.dumps(item, separators=(",", ":")) + "\n" for item in records),
        encoding="utf-8",
    )


def test_analyzes_responses_and_excludes_tool_runtime(tmp_path: Path) -> None:
    rollout = tmp_path / f"rollout-{SESSION_ID}.jsonl"
    write_rollout(rollout, sample_records())

    result = analyze_rollout(rollout, SESSION_ID)

    assert len(result.samples) == 2
    assert result.codex_home == tmp_path.resolve()
    first, second = result.samples
    assert first.elapsed_seconds == pytest.approx(4.0)
    assert first.effective_tps == pytest.approx(25.0)
    assert first.reasoning_tps == pytest.approx(10.0)
    assert first.visible_tps == pytest.approx(15.0)
    assert first.model == "gpt-test"
    assert first.reasoning_effort == "high"
    assert second.elapsed_seconds == pytest.approx(4.0)
    assert second.effective_tps == pytest.approx(20.0)
    assert second.turn_response_index == 2


def test_finds_active_and_archived_rollouts(tmp_path: Path) -> None:
    active = tmp_path / "sessions" / "2026" / "08" / "13"
    rollout = active / f"rollout-{SESSION_ID}.jsonl"
    write_rollout(rollout, sample_records())
    assert find_rollout(tmp_path, SESSION_ID) == rollout.resolve()

    rollout.unlink()
    archived = tmp_path / "archived_sessions" / f"rollout-{SESSION_ID}.jsonl"
    write_rollout(archived, sample_records())
    assert find_rollout(tmp_path, SESSION_ID) == archived.resolve()


def test_codex_home_precedence(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    environment_home = tmp_path / "environment"
    explicit_home = tmp_path / "explicit"
    monkeypatch.setenv("CODEX_HOME", str(environment_home))
    assert resolve_codex_home() == environment_home.resolve()
    assert resolve_codex_home(explicit_home) == explicit_home.resolve()


def test_ignores_incomplete_final_record(tmp_path: Path) -> None:
    rollout = tmp_path / f"rollout-{SESSION_ID}.jsonl"
    write_rollout(rollout, sample_records())
    with rollout.open("a", encoding="utf-8") as stream:
        stream.write('{"timestamp":"unfinished"')

    result = analyze_rollout(rollout, SESSION_ID)

    assert len(result.samples) == 2
    assert "incomplete final JSONL record" in result.warnings[-1]


def test_rejects_invalid_session_id(tmp_path: Path) -> None:
    with pytest.raises(CodexTpsError, match="invalid Codex session ID"):
        find_rollout(tmp_path, "not-a-session")


def test_reports_missing_and_duplicate_sessions(tmp_path: Path) -> None:
    with pytest.raises(CodexTpsError, match="was not found"):
        find_rollout(tmp_path, SESSION_ID)

    active = tmp_path / "sessions" / f"rollout-{SESSION_ID}.jsonl"
    archived = tmp_path / "archived_sessions" / f"rollout-{SESSION_ID}.jsonl"
    write_rollout(active, sample_records())
    write_rollout(archived, sample_records())
    with pytest.raises(CodexTpsError, match="more than one rollout"):
        find_rollout(tmp_path, SESSION_ID)


def test_rejects_legacy_rollout_timing(tmp_path: Path) -> None:
    records = [item for item in sample_records() if item["payload"].get("type") != "task_started"]
    rollout = tmp_path / f"rollout-{SESSION_ID}.jsonl"
    write_rollout(rollout, records)

    with pytest.raises(CodexTpsError, match="legacy rollout"):
        analyze_rollout(rollout, SESSION_ID)


def test_rejects_complete_malformed_json(tmp_path: Path) -> None:
    rollout = tmp_path / f"rollout-{SESSION_ID}.jsonl"
    rollout.write_text('{"bad": }\n', encoding="utf-8")

    with pytest.raises(CodexTpsError, match="invalid JSON"):
        analyze_rollout(rollout, SESSION_ID)
