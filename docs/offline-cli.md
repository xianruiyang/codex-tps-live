# codex-tps 离线命令

[简体中文](offline-cli.md) | [English](offline-cli.en.md)

`codex-tps` 将本地 Codex 会话转换为逐次模型响应的 CSV 和有效输出 TPS 的 PNG 图表。它是原生 Go 可执行文件，运行时不需要语言运行环境，不调用 API，也不发送遥测。

本分支另含 **Codex TPS Live**，提供只读的 Windows 本地日志增量监控面板，按对话、轮次和模型响应展示统计。请参阅 [Windows 实时面板说明](../README.md)。本页介绍的上游离线命令保留原有接口。

## 安装

离线 CLI 可从 [上游 Releases](https://github.com/adenta/codex-tps/releases) 获取适合平台的版本，或使用 Go 安装：

```sh
go install github.com/adenta/codex-tps/cmd/codex-tps@latest
```

本仓库的 Release 发布 Windows 实时面板便携包。上游文档提到的 `codex-ops` 是 Andrew 管理 Mac 的安装方式，本项目不依赖它。

## 用法

```sh
codex-tps 019fec05-c5ab-76e0-ab90-5abe5d5c86ad
codex-tps SESSION_ID --codex-home ~/.codex-fast-desktop
codex-tps SESSION_ID --output-dir ./reports
```

## 输出

每次执行恰好生成两个报告文件：

```text
codex-tps-SESSION_ID.csv
codex-tps-SESSION_ID.png
```

CSV 的每一行对应一次模型采样周期。1600×900 的 PNG 使用实际 UTC 完成时间，堆叠展示可见输出与推理吞吐量，并标出总 TPS 和最近七次响应的滚动中位数。

## “有效 TPS”的含义

Codex 会话日志记录了准确的逐次响应用量，但没有完整的逐 token 时间轨迹。因此本工具计算：

```text
有效 TPS = 输出 token 数 / 模型响应耗时
```

响应时间窗口从最近一次输入或上一个用量边界之后开始，到最后一个模型生成项结束。它排除工具执行时间，包含首 token 等待时间，适合比较实际 Codex 响应速度，但不等同于纯解码速度基准。

`output_tokens` 包含推理 token。报告另提供：

- `reasoning_output_tokens`：推理输出 token。
- `visible_output_tokens = output_tokens - reasoning_output_tokens`：可见输出 token。
- 总输出 TPS、推理 TPS 和可见输出 TPS。

## Codex 数据目录

目录查找优先级如下：

1. `--codex-home PATH`
2. 环境变量 `CODEX_HOME`
3. `~/.codex`

程序同时查找活跃日志 `sessions/` 和归档日志 `archived_sessions/`，因此同一条命令也可用于独立配置的 Codex Desktop 安装。

## 开发

```sh
go test ./...
go vet ./...
```

测试仅使用合成会话日志。会话 JSONL 是 Codex 内部格式，可能变化；不支持或不完整的记录会明确提示。对于缺少可靠响应计时的旧日志，程序拒绝计算，而不会给出容易误导的 TPS。

## 开源协议

[MIT 英文原文](../LICENSE)；另附 [中文译文](../LICENSE.zh-CN.md)。
