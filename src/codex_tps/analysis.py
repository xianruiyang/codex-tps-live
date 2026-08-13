"""Locate and analyze Codex rollout JSONL files.

Codex records exact token usage at the end of each model sampling cycle. The
usage event can be written after a tool finishes, so this module deliberately
uses the last model-generated item as the response end. That keeps tool runtime
out of the throughput calculation.
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
import json
import os
from pathlib import Path
import re
from typing import Any, Iterable


SESSION_ID_PATTERN = re.compile(
    r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-"
    r"[0-9a-fA-F]{4}-[0-9a-fA-F]{12}"
)


class CodexTpsError(Exception):
    """A user-facing analysis error."""


@dataclass(frozen=True)
class ResponseSample:
    session_id: str
    turn_id: str
    response_index: int
    turn_response_index: int
    model: str
    reasoning_effort: str
    started_at: datetime
    completed_at: datetime
    usage_recorded_at: datetime
    elapsed_seconds: float
    input_tokens: int
    cached_input_tokens: int
    cache_write_input_tokens: int
    output_tokens: int
    reasoning_output_tokens: int
    visible_output_tokens: int
    total_tokens: int

    @property
    def effective_tps(self) -> float:
        return self.output_tokens / self.elapsed_seconds

    @property
    def visible_tps(self) -> float:
        return self.visible_output_tokens / self.elapsed_seconds

    @property
    def reasoning_tps(self) -> float:
        return self.reasoning_output_tokens / self.elapsed_seconds


@dataclass(frozen=True)
class AnalysisResult:
    session_id: str
    codex_home: Path
    rollout_path: Path
    samples: tuple[ResponseSample, ...]
    warnings: tuple[str, ...]


def resolve_codex_home(explicit: Path | None = None) -> Path:
    """Resolve the Codex data directory using CLI, environment, then default."""

    if explicit is not None:
        return explicit.expanduser().resolve()
    if configured := os.environ.get("CODEX_HOME"):
        return Path(configured).expanduser().resolve()
    return (Path.home() / ".codex").resolve()


def find_rollout(codex_home: Path, session_id: str) -> Path:
    """Find one active or archived rollout by exact session ID."""

    if SESSION_ID_PATTERN.fullmatch(session_id) is None:
        raise CodexTpsError(f"invalid Codex session ID: {session_id!r}")
    if not codex_home.is_dir():
        raise CodexTpsError(f"Codex home does not exist: {codex_home}")

    matches: set[Path] = set()
    for directory_name in ("sessions", "archived_sessions"):
        directory = codex_home / directory_name
        if not directory.is_dir():
            continue
        matches.update(
            path.resolve()
            for path in directory.rglob(f"*{session_id}*.jsonl")
            if path.is_file()
        )

    if not matches:
        raise CodexTpsError(
            f"session {session_id} was not found under {codex_home}"
        )
    if len(matches) > 1:
        rendered = "\n".join(f"  - {path}" for path in sorted(matches))
        raise CodexTpsError(
            f"session {session_id} matched more than one rollout:\n{rendered}"
        )
    return matches.pop()


def analyze_rollout(
    rollout_path: Path,
    expected_session_id: str,
    codex_home: Path | None = None,
) -> AnalysisResult:
    """Produce one effective-throughput sample for each measurable response."""

    samples: list[ResponseSample] = []
    warnings: list[str] = []
    session_id = expected_session_id
    turn_id = ""
    model = ""
    reasoning_effort = ""
    turn_response_index = 0
    saw_task_started = False

    sampling_start: datetime | None = None
    generated_end: datetime | None = None

    for line_number, row in _iter_jsonl(rollout_path, warnings):
        timestamp_text = row.get("timestamp")
        if not isinstance(timestamp_text, str):
            continue
        try:
            timestamp = _parse_timestamp(timestamp_text)
        except ValueError:
            warnings.append(f"line {line_number}: skipped invalid timestamp")
            continue

        record_type = row.get("type")
        payload = row.get("payload")
        if not isinstance(payload, dict):
            payload = {}
        payload_type = payload.get("type")

        if record_type == "session_meta":
            recorded_id = payload.get("id") or payload.get("session_id")
            if isinstance(recorded_id, str):
                if recorded_id != expected_session_id:
                    raise CodexTpsError(
                        "rollout session ID does not match the requested session: "
                        f"{recorded_id} != {expected_session_id}"
                    )
                session_id = recorded_id
            continue

        if record_type == "turn_context":
            if isinstance(payload.get("turn_id"), str):
                turn_id = payload["turn_id"]
            if isinstance(payload.get("model"), str):
                model = payload["model"]
            effort = payload.get("effort")
            if isinstance(effort, str):
                reasoning_effort = effort
            if generated_end is None:
                sampling_start = timestamp
            continue

        if record_type == "event_msg" and payload_type == "task_started":
            saw_task_started = True
            if isinstance(payload.get("turn_id"), str):
                turn_id = payload["turn_id"]
            sampling_start = timestamp
            generated_end = None
            turn_response_index = 0
            continue

        if record_type == "event_msg" and payload_type == "user_message":
            if generated_end is None:
                sampling_start = timestamp
            continue

        if record_type == "response_item":
            if _is_generated_item(payload):
                generated_end = timestamp
            elif generated_end is None and _is_input_item(payload):
                sampling_start = timestamp
            continue

        if record_type != "event_msg" or payload_type != "token_count":
            continue

        info = payload.get("info")
        if not isinstance(info, dict):
            warnings.append(f"line {line_number}: skipped token count without usage")
            sampling_start = timestamp
            generated_end = None
            continue

        usage = info.get("last_token_usage")
        if not isinstance(usage, dict):
            warnings.append(f"line {line_number}: skipped token count without last usage")
            sampling_start = timestamp
            generated_end = None
            continue

        if sampling_start is None or generated_end is None:
            warnings.append(
                f"line {line_number}: skipped usage without a measurable response window"
            )
            sampling_start = timestamp
            generated_end = None
            continue

        elapsed = (generated_end - sampling_start).total_seconds()
        if elapsed <= 0:
            warnings.append(f"line {line_number}: skipped non-positive response duration")
            sampling_start = timestamp
            generated_end = None
            continue

        output_tokens = _integer(usage, "output_tokens")
        reasoning_tokens = _integer(usage, "reasoning_output_tokens")
        if output_tokens <= 0:
            warnings.append(f"line {line_number}: skipped response without output tokens")
            sampling_start = timestamp
            generated_end = None
            continue
        if reasoning_tokens > output_tokens:
            warnings.append(
                f"line {line_number}: reasoning tokens exceeded output tokens; "
                "visible tokens were clamped to zero"
            )

        turn_response_index += 1
        samples.append(
            ResponseSample(
                session_id=session_id,
                turn_id=turn_id,
                response_index=len(samples) + 1,
                turn_response_index=turn_response_index,
                model=model,
                reasoning_effort=reasoning_effort,
                started_at=sampling_start,
                completed_at=generated_end,
                usage_recorded_at=timestamp,
                elapsed_seconds=elapsed,
                input_tokens=_integer(usage, "input_tokens"),
                cached_input_tokens=_integer(usage, "cached_input_tokens"),
                cache_write_input_tokens=_integer(usage, "cache_write_input_tokens"),
                output_tokens=output_tokens,
                reasoning_output_tokens=reasoning_tokens,
                visible_output_tokens=max(output_tokens - reasoning_tokens, 0),
                total_tokens=_integer(usage, "total_tokens"),
            )
        )
        sampling_start = timestamp
        generated_end = None

    if not saw_task_started:
        raise CodexTpsError(
            "this legacy rollout predates reliable per-response timing; "
            "codex-tps cannot calculate trustworthy TPS for it"
        )
    if not samples:
        raise CodexTpsError(
            "the rollout did not contain any measurable model responses with token usage"
        )

    return AnalysisResult(
        session_id=session_id,
        codex_home=(codex_home or _infer_codex_home(rollout_path)).resolve(),
        rollout_path=rollout_path,
        samples=tuple(samples),
        warnings=tuple(warnings),
    )


def _iter_jsonl(
    path: Path, warnings: list[str]
) -> Iterable[tuple[int, dict[str, Any]]]:
    try:
        stream = path.open("r", encoding="utf-8")
    except OSError as error:
        raise CodexTpsError(f"could not open rollout {path}: {error}") from error

    with stream:
        for line_number, line in enumerate(stream, start=1):
            if not line.strip():
                continue
            try:
                row = json.loads(line)
            except json.JSONDecodeError as error:
                if not line.endswith("\n"):
                    warnings.append(
                        f"line {line_number}: ignored an incomplete final JSONL record"
                    )
                    return
                raise CodexTpsError(
                    f"invalid JSON in {path} at line {line_number}: {error.msg}"
                ) from error
            if not isinstance(row, dict):
                raise CodexTpsError(
                    f"invalid JSONL object in {path} at line {line_number}"
                )
            yield line_number, row


def _parse_timestamp(value: str) -> datetime:
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def _infer_codex_home(rollout_path: Path) -> Path:
    for parent in rollout_path.parents:
        if parent.name in {"sessions", "archived_sessions"}:
            return parent.parent
    return rollout_path.parent


def _integer(mapping: dict[str, Any], key: str) -> int:
    value = mapping.get(key, 0)
    return value if isinstance(value, int) and not isinstance(value, bool) else 0


def _is_generated_item(payload: dict[str, Any]) -> bool:
    item_type = payload.get("type")
    if item_type == "reasoning":
        return True
    if item_type == "message" and payload.get("role") == "assistant":
        return True
    return isinstance(item_type, str) and item_type.endswith("_call")


def _is_input_item(payload: dict[str, Any]) -> bool:
    item_type = payload.get("type")
    if item_type in {"function_call_output", "custom_tool_call_output", "agent_message"}:
        return True
    return item_type == "message" and payload.get("role") in {
        "user",
        "developer",
        "system",
    }
