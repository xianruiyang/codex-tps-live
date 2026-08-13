# codex-tps

`codex-tps` turns a local Codex session into a per-response CSV and a PNG chart
of effective output tokens per second.

It reads the rollout JSONL already stored in your Codex home. It does not call
an API, send telemetry, or include prompts, messages, tool arguments, or tool
results in its outputs.

## Run it with UV

After cloning the repository:

```bash
uv sync
uv run codex-tps 019fec05-c5ab-76e0-ab90-5abe5d5c86ad
```

For a non-default Codex home:

```bash
uv run codex-tps 019ff339-7cf5-7273-8346-00319d1cc68b \
  --codex-home ~/.codex-fast-desktop
```

Choose a destination with `--output-dir`:

```bash
uv run codex-tps SESSION_ID --output-dir ./reports
```

The equivalent module entry point is:

```bash
uv run python -m codex_tps SESSION_ID
```

You can also install the command through UV:

```bash
uv tool install .
codex-tps SESSION_ID
```

## Outputs

Each invocation writes exactly two report files:

```text
codex-tps-SESSION_ID.csv
codex-tps-SESSION_ID.png
```

The CSV contains one row per model sampling cycle. The PNG uses real UTC time
on the x-axis and effective output tokens per second on the y-axis. Each event
mark stacks visible output TPS and reasoning TPS; a seven-response rolling
median shows the local trend. Idle periods remain blank. The chart header prints
the resolved Codex home so reports from isolated installations remain identifiable.

## What “effective TPS” means

Codex rollouts contain exact per-response token usage but not a complete
token-by-token timing trace. This utility therefore calculates:

```text
effective TPS = output tokens / model-response elapsed time
```

The response window starts after the latest input or previous usage boundary
and ends at the last model-generated item. Tool execution time is excluded.
First-token latency is included. This makes the result useful for comparing
real Codex responsiveness, but it is not a pure decoder-only benchmark.

`output_tokens` includes reasoning tokens. The report also exposes:

- `reasoning_output_tokens`
- `visible_output_tokens = output_tokens - reasoning_output_tokens`
- total, reasoning, and visible TPS

## Codex homes

The lookup order is:

1. `--codex-home PATH`
2. the `CODEX_HOME` environment variable
3. `~/.codex`

Both active `sessions/` and `archived_sessions/` are searched. This makes the
same command usable with isolated Codex Desktop installations.

## Development

```bash
uv sync --dev
uv run pytest
```

Tests use synthetic rollouts only. The rollout JSONL format is internal to
Codex and may evolve; unsupported or incomplete records are reported clearly.
Legacy rollouts that predate reliable response timing are rejected rather than
reported with misleading TPS values.

## License

MIT
