# codex-tps

`codex-tps` turns a local Codex session into a per-response CSV and a PNG chart
of effective output tokens per second. It is a native Go executable with no
language runtime, API calls, or telemetry.

## Install

Download the archive for your platform from GitHub Releases, or build it with:

```sh
go install github.com/adenta/codex-tps/cmd/codex-tps@latest
```

Andrew's managed Macs install the pinned executable through `codex-ops`.

## Usage

```sh
codex-tps 019fec05-c5ab-76e0-ab90-5abe5d5c86ad
codex-tps SESSION_ID --codex-home ~/.codex-fast-desktop
codex-tps SESSION_ID --output-dir ./reports
```

## Outputs

Each invocation writes exactly two report files:

```text
codex-tps-SESSION_ID.csv
codex-tps-SESSION_ID.png
```

The CSV contains one row per model sampling cycle. The 1600×900 PNG uses real
UTC completion time and stacks visible and reasoning throughput, with total TPS
markers and a seven-response rolling median.

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

```sh
go test ./...
go vet ./...
```

Tests use synthetic rollouts only. The rollout JSONL format is internal to
Codex and may evolve; unsupported or incomplete records are reported clearly.
Legacy rollouts that predate reliable response timing are rejected rather than
reported with misleading TPS values.

## License

MIT
