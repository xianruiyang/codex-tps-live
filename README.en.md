# Codex TPS Live

[简体中文](README.md) | [English](README.en.md)

A local Windows dashboard for Codex usage and speed. It reads local logs to show tokens, cache usage, and TPS by conversation, turn, and model response, with speed trends, quota history, and capacity estimates.

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

# Choose a quota-history directory and local namespace (not account identification)
.\codex-tps-live.exe --data-dir 'D:\monitor-data' --quota-profile work --open
```

At startup, discovery defaults to `sessions` files updated within the last seven days, processed from newest to oldest by file modification time. Discovered sessions remain in the current process; new files are discovered every five seconds. Incremental reads and dashboard refreshes default to every 0.5 seconds. Initial loading locates complete boundaries for the latest ten turns from the file tail and reads the preceding cumulative usage checkpoint as a baseline, then parses the selected turns chronologically. Increasing N loads older turns as needed, retaining existing data and showing progress. Decreasing N cancels unnecessary backfill. Raw logs are not copied. Conversation statistics rebuild from the selected logs after restart, while the quota page separately persists necessary metering records. Older logs without reliable turn boundaries or a preceding cumulative checkpoint fall back to batched parsing from the beginning.

## Dashboard

- Sidebar entries show the model and reasoning effort currently recorded in the logs. Subagents prefer the name in their task path, with nickname, role, and task title below; details show the full task path. Search supports names, nicknames, roles, titles, models, working directories, and IDs.
- Service-tier labels appear beside models in conversations, turns, and individual responses: `ultra fast`, `fast`, `default`, and `unknow`. Inferred subagent settings are marked as inherited. Historical responses prefer their own records; inherited values match the main conversation's settings at the start of that turn.
- Independent All / Main conversations / Subagents filters can be combined with the active-status filter. Parent and child usage are not merged.
- The latest N turns setting defaults to **10**, including active turns. Sidebar usage, cards, turn lists, and CSV exports use this same scope. Settings are saved in the browser's local site storage.
- Conversations sort by the reporting time of their latest newly recorded, deduplicated model response, newest first. Ordinary log events, status changes, and model settings update the display without changing the sort timestamp. New conversations without responses use creation time; IDs provide a stable tie-breaker. Sorting is independent of N, and loading older history does not replace the latest response time. Refreshes reuse existing entries, updating content and order only when needed.
- Turns and responses appear newest first with normal positive numbering. Until all history is loaded, turn numbering starts at the earliest loaded turn and is rebuilt after older history is loaded. Hover over a turn label to see the numbering scope. Loaded turn counts are not presented as whole-conversation totals.
- Each turn shows input, cache hits, cache writes, output, estimated credits, output TPS, and model-response duration. Expand it to inspect individual responses and their estimated credit cost.
- Sidebar conversations and the selected conversation's summary show estimated credits for the latest N turns. A turn sums its responses; a conversation sums the turns in that window, excluding subagent usage. Each response is priced using its recorded model, cache split, output, and resolved historical speed tier, rather than the conversation's current settings. Missing model or speed data remains unpriced. Partial totals show coverage instead of treating unknown consumption as zero. Dashboard and quota pages share the selected credit basis and context scenario across navigation. These are equivalent-credit estimates, not official billing records.
- Credit values in conversations, turns, responses, and the quota page display exactly two decimal places. Internal pricing, aggregation, and API data retain their original precision.
- The turn table shows time to first token (TTFT), read directly from the completion event's `time_to_first_token_ms` and displayed in seconds with millisecond precision. Active turns show a pending value; unavailable measurements show a dash. This applies to main conversations and subagents. The log reports one value per turn after completion and does not clearly distinguish first visible text, reasoning, or a tool call. TTFT is not assigned to individual responses or subtracted from the TPS denominator.
- Multiple model responses within a turn are added together. Subagents remain separate conversations and are not added to their parents.
- The main card shows the latest response's TPS and the age of its usage report, plus average TPS across the latest N turns. Reasoning/visible output breakdowns and the logged usage rate over the last 30 seconds are also available.
- CSV exports contain response details for the selected conversation's latest N turns, including `turn_id` and the response index within its turn. During backfill, the button states the loaded turn count and exports currently loaded responses. The N-turn scope resumes automatically when loading completes.
- Display pause, light/dark themes, and narrow layouts are supported. Pausing the display does not stop collection.
- All three pages keep the same Conversation details / Speed trends / Quota trends navigation, with the current page highlighted and pause/theme actions separated. Navigation stays visible while scrolling long details. Speed trends use one chart per model, grouped by reasoning effort and service tier. Both lines with markers and scatter plots are available.

## Speed trends

The `/trends` page defaults to the latest **24 hours**, with presets for 1, 6, 12, 18, and 24 hours, three days, and seven days, plus custom start/end times. Filter by model, reasoning effort, service tier, and main conversation/subagent, and switch between output TPS and visible-output TPS. The horizontal axis uses usage-report timestamps with scale-dependent ticks. The full-view vertical axis starts at zero.

Each chart has independent controls. Use the mouse wheel inside the plot to zoom both axes around the cursor; hold Shift to zoom only the vertical axis, or Ctrl to zoom only the horizontal axis. Drag with the left mouse button to pan. The default vertical axis starts at zero and follows the visible aggregate trends, so isolated raw outliers do not stretch it. Double-click or choose fit to adjust both axes to the visible trends in the current time view, with padding; with filtering off, fit uses raw points instead. If that time view has no samples, fit uses all visible data in the chart. Reset clears zoom and pan, restoring the complete selected time range and the initial vertical scale. A separate Show all scatter points button expands the view to include raw outliers; Reset returns to the trend scale. The chart reports the number of raw points outside the current vertical range. The +/− buttons also control zoom, and zoom factors are relative to the default view. Zooming and panning may leave empty space beyond the data bounds to keep the value under the cursor fixed. They only change the view, without extending the selected log period or changing the filter window or raw data. Views survive live refreshes and resizing, and return to the default scale when time or data filters change. The wheel outside the plot retains normal page scrolling and browser shortcuts.

Filtering defaults to a **10-minute window**, configurable from 1 to 1440 minutes. The recommended duration-weighted throughput is total output tokens divided by total response duration within each model/effort/tier window. Visible TPS uses non-reasoning output tokens. This is a response-duration-weighted average, so longer responses carry more weight. Durations of concurrent conversations are added separately; this is not total throughput divided by wall-clock time. Median, equal-weight mean, and no filtering are also available. Settings are saved locally in the browser.

Each aggregate point is placed at the center of its time bucket, with size indicating accumulated response duration. Hover for sample count, tokens, duration weight, and the raw TPS range. The current bucket updates as new responses arrive. Empty buckets are not filled with zeroes. Lines break at empty buckets or when actual samples in adjacent buckets are separated by more than one window. Connecting lines help show changes between buckets; they do not imply continuous generation.

Faint raw scatter points are overlaid by default and can be disabled without changing the current zoom range. Hover to inspect individual deduplicated usage records with reliable timing. Click a legend entry to hide or restore its effort/tier curve and scatter points. Legends stay visible; hidden-series settings survive refreshes and are independent for each model. Toggling a legend preserves the current time view and recalculates the vertical range. Fit uses only visible series, and zoom and fit controls are disabled when all series are hidden. Disabling filtering draws raw scatter points without connecting them. Raw output TPS includes reasoning and first-token waiting, using the same accounting as the conversation details. Responses without reliable timing are counted separately instead of plotted as zeroes. Filtering changes only chart display, not collected data or conversation details.

Charts load history for their selected time range independently of the dashboard's N-turn limit. When older data is needed, discovery expands and loads progressively from complete turn boundaries. Existing charts remain visible with a notice until the requested range has finished loading. The main dashboard retains its own N-turn scope. Archived logs remain controlled by the startup option `--include-archived`.

Charts refresh once per second while background log polling keeps its configured interval. Charts, scripts, and styles are embedded in the portable executable. At runtime the page accesses only `127.0.0.1`; no chart-library installation or internet connection is needed.

## Quota trends and capacity estimates

The separate `/quota` page reads local `token_count.rate_limits` snapshots, without a separate monitor login or network requests. It defaults to 24 hours, with 1-hour, 6-hour, 12-hour, 24-hour, 7-day, and 30-day presets. Start/end fields are always visible: apply a custom range to fix the local-time bounds, or choose a preset for a rolling range. Remaining/used percentages are drawn as steps for each `limit_id` and its actual primary or secondary window. Fractional percentages are preserved when reported; integer-only data stays integer, without invented precision. The page shows report age, window length, reset time, and backfill progress. Repeated snapshots are not added together. Missing windows stay unavailable, and reaching a reset timestamp prompts for a new report rather than assuming 100% has returned. Small reset-timestamp jitter does not create a new window.

Quota snapshots and per-response model, speed, and token counters are saved under the current computer's `%LOCALAPPDATA%\CodexTPSLive`, in JSONL histories separated by Codex home and `--quota-profile`. Override the location with `--data-dir`. Writes are batched about every two seconds and flushed on normal exit; stable checkpoint keys deduplicate replay after restart. History backfill is independent of N, and recorded quota history survives a smaller N or moved source logs. Records contain no prompts, replies, tool output, or credentials, and are not written to the Codex log directory. Write failures are visible in the page, with pending records kept in memory for retry. A quota profile is only a local namespace, not proof of account identity. After switching accounts, choose another profile and ensure its source logs belong to that account. Moving the chart start does not isolate the history used for capacity calibration.

Model costs use the [official Codex standard credit table](https://learn.chatgpt.com/docs/pricing), checked on 2026-10-15, with separate uncached input, cached input, and output rates. Every request's full context counts as input. Reasoning is already included in output, and Codex credits have no extra cache-write charge. Reasoning effort affects actual tokens rather than adding another multiplier. The [official speed rules](https://learn.chatgpt.com/docs/agent-configuration/speed) distinguish included subscription usage (Fast 2.5×, Ultrafast 8×) from purchased credits (2× and 6×). Select the applicable basis on the page. Unrecognized models or speed tiers are not treated as free responses.

The default uses the Codex standard credit table, without assuming an unconfirmed long-context subscription surcharge. An optional API long-context scenario applies 2× input/cache and 1.5× output to the entire request when input exceeds **272,000** tokens, then applies the speed multiplier. This comes from [API guidance](https://developers.openai.com/api/docs/pricing) and is explicitly labeled as an assumption, not an official Codex subscription rule. Full context, cache usage, and actual output still count in either scenario.

Estimated full capacity is priced response cost across complete intervals after the calibration baseline, divided by their increase in used percentage points, multiplied by 100. Capacity calibration uses **the latest continuous quota segment in collected history through the selected end time**. Moving the chart start or changing its span no longer removes recorded calibration samples or rebuilds the baseline. The chart range limits the curve, change details, and target-model mixed-token samples; changing that mix can still change token capacity. Only collected history is used, without claiming complete coverage of the quota cycle. Newly backfilled records can still update the estimate. Calibration dates, response coverage, and model costs are shown separately and may precede the chart start.

The first observed positive percentage change in each segment establishes its baseline. The initial interval may be incomplete, so its credits, percentage delta, response coverage, and interval count are excluded from calibration. If it falls within the chart range, it remains visible in the curve and change details, marked as an initial interval excluded from calibration. Actual usage and estimated credit consumption in conversation details are unaffected. One further positive change is needed to form a complete interval before capacity can be estimated.

A sustained rollback, changed plan/window, reset-time change over 120 seconds, or reporting gap over six hours starts a new segment and repeats this baseline procedure; segments are never combined. Concurrent conversations can briefly report an older value: a same-window rollback must occur within two minutes of the previous accepted snapshot, with low reports spanning no more than two minutes. Once recovery is observed, the rollback remains a grey hollow point instead of starting another calibration baseline. Recovery may wait for the next sparse report, but must stay within six hours of the previous accepted snapshot and the same window. For example, an isolated `34% → 33%` followed by `34%` five minutes later no longer resets the baseline merely because no reports arrived in between. **Rollback size is not limited to one percentage point**. Its response tokens still count within complete calibration intervals. Recovery is not assumed before it is observed, when low reports persist beyond two minutes, or across the plan, window, or reset-time changes described above. A capacity range reflects roughly ±1 percentage point of endpoint resolution, not a statistical confidence interval. Small spans remain provisional. Response coverage, model costs, and individual percentage changes are shown. Incomplete backfill or unpriced responses can bias the result downward and leave the upper bound unknown. Without a complete interval and priced usage, the curve remains available while estimation waits for more data.

Choose a target model and speed to see two primary cards: **estimated full credits / estimated remaining credits**, each paired with its **estimated mixed-token capacity**. Credits are visible immediately without expanding details. Each token capacity combines uncached input, cached input, and output. Repeated context counts as processed input, not just new text. Composition uses summed token counts from valid records within the selected time range and quota bucket, rather than averaging per-request percentages. With at least 10 responses, the target model supplies its own mix. Otherwise, the app uses whichever of `gpt-6-astra` and `gpt-6.1-sol` has more actual records. A reference with fewer than 10 responses is marked as a small sample. If neither reference has records, the target's smaller sample is used; if all are absent, estimation waits rather than assuming a fixed cache/output split. Ten responses is a source-selection threshold, not a confidence claim. The page displays the mix, reference model, response count, and sample time range.

Each reference request is repriced at the target model and speed, retaining its actual context and cache split. Summed cost divided by total mixed tokens gives the cost per million mixed tokens. Dividing full/remaining equivalent credits by that cost gives the two capacities, without first rounding down a request count. The optional long-context scenario is applied per request, never to an average context size. Credit calibration, model costs, percentage changes, and rules are available in expandable details. Estimates assume the selected local logs belong to one account and cover the relevant consumption. Other computers, cloud tasks, shared features, and delayed server reports can change the result. Equivalent credits are not a server-published subscription balance; the purchased-credit table alone does not determine an included plan's total allowance. Estimates are not presented as an official bill.

The quota chart also shows an **estimated exhaustion time**, using the last six hours of observed quota consumption by default. Choose 1, 6, 12, or 24 hours, or the full chart range. Drag horizontally in the plot to select a reference interval; calculation commits only on release. Manual time fields provide an alternative, and Escape cancels an active drag. Custom selections stay fixed while presets roll with the chart; changing the chart bounds restores the preset reference duration. Dragging only updates the selection overlay, without recalculating on every movement or changing chart bounds and credit calibration.

Average consumption is the difference in used percentage points between the first and last valid snapshots in the selection, divided by their actual elapsed time. Estimated exhaustion is the latest valid snapshot time plus remaining percentage points divided by that rate. This calculation uses original quota snapshots before chart compaction, independently of credit pricing and token capacity. It displays actual sample bounds, hourly consumption, and the data timestamp. Only the current continuous segment is used, with confirmed stale reports excluded. Reset/rollback/gap boundaries require a new selection; insufficient samples or no observed consumption produce no invented date. When a reset comes first, the page explicitly notes that the current cycle is expected to reset before exhaustion and that the displayed date extrapolates without resetting. Percentage rounding, backfill, and future workload changes can affect the result; this is not an official quota guarantee.

## Logs and names

Conversation titles prefer the latest nonempty name in `session_index.jsonl` under the resolved Codex home, selecting by `updated_at` for each ID. Changes to the local index refresh these names, including forked conversations and renames. Without an index name, conversation titles fall back to a short preview of the first valid user text. Turn titles still use valid user-text previews, skipping injected environment, workspace rules, and Referenced chats notices. If a subagent has no title, its task name is used, then its nickname; actual titles are retained. The sidebar avoids repeating a title identical to the name. Full conversations and tool output are not shown. Older logs lacking valid times or turn boundaries show warnings, unassigned usage, or unavailable TPS instead of fabricated zeroes.

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
| `web/` | Embedded dashboard, speed-trends page, and quota page |
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
