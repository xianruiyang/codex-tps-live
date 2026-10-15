# Codex TPS Live

[简体中文](README.md) | [English](README.en.md)

A local Windows dashboard for Codex usage and speed. It reads local logs to show tokens, cache usage, TPS, and time trends by conversation, turn, and model response.

Based on [adenta/codex-tps](https://github.com/adenta/codex-tps) v0.2.0 (`be54961`), preserving its MIT copyright notice and offline command. See the [offline CLI guide](docs/offline-cli.en.md) for that command.

## Run the portable version

[Download the Windows x64 portable package](https://github.com/xianruiyang/codex-tps-live/releases/latest/download/codex-tps-live-windows-amd64.zip), or choose a version under [Releases](https://github.com/xianruiyang/codex-tps-live/releases). Extract the entire archive anywhere and double-click `启动监控.cmd`. Windows 10/11 x64 is supported, including paths with spaces or Chinese characters. Installation and administrator privileges are not required. The launcher starts the local program and opens your default browser. Keep its console running; press Ctrl+C to exit.

The package contains six files: `codex-tps-live.exe`, `启动监控.cmd`, the Chinese guide `使用说明.md`, the English guide `Usage.en.md`, the original English MIT `LICENSE`, and its Chinese translation `LICENSE.zh-CN.md`. Web assets are embedded in the executable. Running it requires no source tree, `web` folder, Go, Node, Python, API key, or internet connection. The package does not include local Codex logs, credentials, or screenshots. The launcher locates the executable relative to itself, independently of drive letters, user names, extraction locations, or the current working directory. It also forwards command-line arguments.

You can also run it from PowerShell:

```powershell
.\codex-tps-live.exe --open
```

The default address is `http://127.0.0.1:8765`. The Codex data directory is selected in this order: `--codex-home`, the current computer's `CODEX_HOME`, then `.codex` under the current Windows user's home directory, usually `%USERPROFILE%\.codex`. No directory from the development machine is required. A custom path should point to the Codex data directory containing `sessions`. If the target computer has no logs yet, the page reports an unavailable directory or no conversations and discovers logs when they appear. There are no CDNs, external fonts, or third-party scripts.

To upgrade, close the old monitor, replace the release files in its directory, and restart it. Your Codex conversations are unaffected. Building from source updates both the ZIP and the unpacked `dist` directory. You can run `dist\启动监控.cmd` directly or copy the entire `dist` directory to another computer.

The program only reads logs. It does not start Codex or app-server, read credentials, proxy requests, or modify Codex requests. The browser accesses only the local listening address. Exiting the program stops monitoring; closing or pausing the page does not stop collection. Monitoring still uses local CPU, memory, and disk resources.

## Options

```powershell
# Use another Codex data directory
.\codex-tps-live.exe --codex-home 'D:\my-codex' --open

# Discover all available history, including archived sessions
.\codex-tps-live.exe --days 0 --include-archived --open

# Pick an available port automatically
.\codex-tps-live.exe --port 0 --open

# Change the log polling interval
.\codex-tps-live.exe --interval 2s --open
```

At startup, discovery defaults to `sessions` files updated within the last seven days, processed from newest to oldest by file modification time. Discovered sessions remain in the current process; new files are discovered every five seconds. Incremental reads and dashboard refreshes default to every 0.5 seconds. Initial loading locates complete boundaries for the latest ten turns from the file tail and reads the preceding cumulative usage checkpoint as a baseline, then parses the selected turns chronologically. Increasing N loads older turns as needed, retaining existing data and showing progress. Decreasing N cancels unnecessary backfill. The program does not write raw logs or a usage ledger to another directory; it rebuilds statistics from the selected logs after restart. Older logs without reliable turn boundaries or a preceding cumulative checkpoint fall back to batched parsing from the beginning.

## Dashboard

- Sidebar entries show the model and reasoning effort currently recorded in the logs. Subagents prefer the name in their task path, with nickname, role, and task title below; details show the full task path. Search supports names, nicknames, roles, titles, models, working directories, and IDs.
- Service-tier labels appear beside models in conversations, turns, and individual responses: `ultra fast`, `fast`, `default`, and `unknow`. Inferred subagent settings are marked as inherited. Historical responses prefer their own records; inherited values match the main conversation's settings at the start of that turn.
- Independent All / Main conversations / Subagents filters can be combined with the active-status filter. Parent and child usage are not merged.
- The latest N turns setting defaults to **10**, including active turns. Sidebar usage, cards, turn lists, and CSV exports use this same scope. Settings are saved in the browser's local site storage.
- Conversations sort by the reporting time of their latest newly recorded, deduplicated model response, newest first. Ordinary log events, status changes, and model settings update the display without changing the sort timestamp. New conversations without responses use creation time; IDs provide a stable tie-breaker. Sorting is independent of N, and loading older history does not replace the latest response time. Refreshes reuse existing entries, updating content and order only when needed.
- Turns and responses appear newest first with normal positive numbering. Until all history is loaded, turn numbering starts at the earliest loaded turn and is rebuilt after older history is loaded. Hover over a turn label to see the numbering scope. Loaded turn counts are not presented as whole-conversation totals.
- Each turn shows input, cache hits, cache writes, output, output TPS, and model-response duration. Expand it to inspect individual responses.
- The turn table shows time to first token (TTFT), read directly from the completion event's `time_to_first_token_ms` and displayed in seconds with millisecond precision. Active turns show a pending value; unavailable measurements show a dash. This applies to main conversations and subagents. The log reports one value per turn after completion and does not clearly distinguish first visible text, reasoning, or a tool call. TTFT is not assigned to individual responses or subtracted from the TPS denominator.
- Multiple model responses within a turn are added together. Subagents remain separate conversations and are not added to their parents.
- The main card shows the latest response's TPS and the age of its usage report, plus average TPS across the latest N turns. Reasoning/visible output breakdowns and the logged usage rate over the last 30 seconds are also available.
- CSV exports contain response details for the selected conversation's latest N turns, including `turn_id` and the response index within its turn. During backfill, the button states the loaded turn count and exports currently loaded responses. The N-turn scope resumes automatically when loading completes.
- Display pause, light/dark themes, and narrow layouts are supported. Pausing the display does not stop collection.
- The speed-trends link opens a separate chart page, with one chart per model and series grouped by reasoning effort and service tier. Both lines with markers and scatter plots are available.

## Speed trends

The `/trends` page defaults to the latest **24 hours**, with presets for 1, 6, 12, 18, and 24 hours, three days, and seven days, plus custom start/end times. Filter by model, reasoning effort, service tier, and main conversation/subagent, and switch between output TPS and visible-output TPS. The horizontal axis uses usage-report timestamps with scale-dependent ticks. The full-view vertical axis starts at zero.

Each chart has independent controls. Use the mouse wheel inside the plot to zoom both axes around the cursor; hold Shift to zoom only the vertical axis, or Ctrl to zoom only the horizontal axis. After zooming, drag with the left mouse button to pan. Double-click or choose fit to adjust both axes to samples within the current time view, adding some padding; if that view has no samples, fit uses all of the chart's data. Reset restores the full view, and +/− buttons also control zoom. Each axis shows its zoom factor. Zooming and panning stay within the selected time range and the chart's full TPS range; they do not change the selected period, filter window, or raw data. Views survive live refreshes and resizing, and reset when time or data filters change. The wheel outside the plot retains normal page scrolling and browser shortcuts.

Filtering defaults to a **10-minute window**, configurable from 1 to 1440 minutes. The recommended duration-weighted throughput is total output tokens divided by total response duration within each model/effort/tier window. Visible TPS uses non-reasoning output tokens. This is a response-duration-weighted average, so longer responses carry more weight. Durations of concurrent conversations are added separately; this is not total throughput divided by wall-clock time. Median, equal-weight mean, and no filtering are also available. Settings are saved locally in the browser.

Each aggregate point is placed at the center of its time bucket, with size indicating accumulated response duration. Hover for sample count, tokens, duration weight, and the raw TPS range. The current bucket updates as new responses arrive. Empty buckets are not filled with zeroes. Lines break at empty buckets or when actual samples in adjacent buckets are separated by more than one window. Connecting lines help show changes between buckets; they do not imply continuous generation.

Faint raw scatter points are overlaid by default and can be disabled. Hover to inspect individual deduplicated usage records with reliable timing. Click a legend entry to hide or restore its effort/tier curve and scatter points. Legends stay visible; hidden-series settings survive refreshes and are independent for each model. Fit uses only visible series. Disabling filtering draws raw scatter points without connecting them. Raw output TPS includes reasoning and first-token waiting, using the same accounting as the conversation details. Responses without reliable timing are counted separately instead of plotted as zeroes. Filtering changes only chart display, not collected data or conversation details.

Charts load history for their selected time range independently of the dashboard's N-turn limit. When older data is needed, discovery expands and loads progressively from complete turn boundaries. Existing charts remain visible with a notice until the requested range has finished loading. The main dashboard retains its own N-turn scope. Archived logs remain controlled by the startup option `--include-archived`.

Charts refresh once per second while background log polling keeps its configured interval. Charts, scripts, and styles are embedded in the portable executable. At runtime the page accesses only `127.0.0.1`; no chart-library installation or internet connection is needed.

## Logs and names

Conversation and turn titles use a short preview of the first user text. If a subagent has no title, its task name is used, then its nickname; actual titles are retained. The sidebar avoids repeating a title identical to the name. Full conversations and tool output are not shown. Older logs lacking valid times or turn boundaries show warnings, unassigned usage, or unavailable TPS instead of fabricated zeroes.

Subagent names come from `agent_path`, `agent_nickname`, and `agent_role` in local `session_meta` records. Top-level fields take priority, with matching fields under `source.subagent.thread_spawn` as fallback. File-header metadata is read even when loading recent turns from the tail, without scanning parent conversations or accessing the network. If only a nickname exists, it is displayed directly. Older logs without a recorded name show that the name is unavailable.

Service tiers come from `thread_settings.service_tier` in local `thread_settings_applied` events: `fast` or `priority` maps to `fast`, `default` to `default`, and `ultrafast` to `ultra fast`. Missing, `auto`, or unrecognized values display `unknow`. Tail loading also reads settings between the selected turn boundary and the preceding cumulative usage checkpoint, preserving their timestamps. The [official Codex subagent rules](https://learn.chatgpt.com/docs/agent-configuration/subagents) describe subagents following the main conversation's service-tier preference. If a subagent has no setting of its own, the program follows parent links to the main conversation and marks the result as inherited. Conversation labels show the main conversation's current preference; historical turns and responses match settings at turn start, rather than reusing today's switch. If the parent log or historical settings are outside the loaded scope, the value stays unknown; increase N or use `--days 0` to load relevant conversations. Global configuration is not used to fill historical values. These labels describe request settings and do not guarantee the server's actual processing tier. The mapping also references the [Codex configuration documentation](https://learn.chatgpt.com/docs/config-file/config-reference) and [official Fast mode guide](https://developers.openai.com/api/docs/guides/fast-mode); these pages are not accessed at runtime.

## Accounting

Input tokens describe context processed by each model request, not just newly typed messages. Cache hits are part of input, and reasoning tokens are part of output; do not add them again. Cache writes are shown separately. Non-cached input means input minus cache hits and does not exclude cache writes.

Output TPS follows codex-tps's **effective output TPS**: output tokens divided by model-response duration measurable from the log. It includes first-token waiting and excludes tool execution between responses. The main card uses the latest response within the selected N turns, displays the reporting time, and indicates when it is waiting for the next usage report during an active turn. An old value is not presented as a current generation rate. Average TPS is the sum of reliably timed output tokens divided by the sum of their response durations, not a direct mean of TPS values. Untimed responses still contribute to token counts but not to either side of average TPS; the page shows timing coverage.

The 30-second usage rate is the tokens reported during the last 30 seconds within the selected turns divided by 30 seconds. It measures usage arriving in the log, which differs from output TPS. Codex usually reports usage after a response completes, so values can jump. Active responses have no reliable token-by-token count; character counts are not used to estimate hidden reasoning or unreported usage. These statistics are local log analysis, not a final bill.

Cumulative checkpoints provide deduplication; repeated checkpoints are not counted again. The program waits for incomplete lines to finish and rebuilds conversation statistics after file truncation, replacement, or migration. Counter resets or missing fields are reported as data limitations. Turn grouping relies on `turn_id` in `task_started`/`turn_context`; one user task can contain multiple model responses.

## Project layout

| Path | Purpose |
| --- | --- |
| `cmd/codex-tps-live/` | Live-monitor entry point |
| `live_*.go` | Log parsing, incremental reads, statistics, and local HTTP service |
| `web/` | Embedded dashboard and speed-trends page |
| `build-portable.ps1` | Build and package the Windows x64 executable |
| `scripts/package-portable.ps1` | Reuse the existing executable to update guides, licenses, and ZIP |
| `README.md` / `README.en.md` | Chinese / English live-monitor guide |
| `docs/offline-cli.md` / `docs/offline-cli.en.md` | Chinese / English upstream offline CLI guide |
| `LICENSE` / `LICENSE.zh-CN.md` | Original English MIT license / Chinese translation |
| `dist/` | Current unpacked portable package; ignored by Git |
| `outputs/` | Local previews and archived builds; ignored by Git |

## Build from source

```powershell
pwsh -File .\build-portable.ps1
```

Building requires PowerShell 7, Go on PATH, and the upstream plotting dependencies. Initial dependency downloads require internet access. If Go is not on PATH, pass `-GoExe` with its executable path. The script locates the source relative to itself, produces the unpacked `dist` directory and portable ZIP, and replaces the current release files. It targets the Windows x64 baseline instruction set with CGO disabled. Build tools are not required on computers running the portable package, and the live monitor makes no outbound requests at runtime. The original `cmd/codex-tps` retains its CSV/PNG offline analysis behavior.

To update only the portable guides, licenses, and archive, reuse the existing executable in `dist` without rebuilding or restarting the monitor:

```powershell
pwsh -File .\scripts\package-portable.ps1
```

Pushing a `v*` tag triggers GitHub Actions on a Windows runner. It calls the same `build-portable.ps1` and publishes the Windows portable ZIP and `SHA256SUMS` to the release. The tag should match the program version in `live_server.go`. GitHub's automatic Source code archives are for development; download `codex-tps-live-windows-amd64.zip` from Release Assets to run the program directly.

## License

This project uses the [original English MIT License](LICENSE), with a [Chinese translation](LICENSE.zh-CN.md) for convenience. The English original governs. The upstream codex-tps copyright is preserved, and the Windows live-monitor extensions are also released under MIT. The license accompanies source and portable distributions.
