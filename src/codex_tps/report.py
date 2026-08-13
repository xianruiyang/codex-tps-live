"""Write CSV and PNG reports for analyzed Codex responses."""

from __future__ import annotations

from collections import deque
import csv
from datetime import datetime
import os
from pathlib import Path
from statistics import median
import tempfile
from typing import Iterable

from codex_tps.analysis import AnalysisResult, ResponseSample


CSV_COLUMNS = (
    "session_id",
    "turn_id",
    "response_index",
    "turn_response_index",
    "model",
    "reasoning_effort",
    "started_at",
    "completed_at",
    "usage_recorded_at",
    "elapsed_seconds",
    "input_tokens",
    "cached_input_tokens",
    "cache_write_input_tokens",
    "output_tokens",
    "reasoning_output_tokens",
    "visible_output_tokens",
    "total_tokens",
    "effective_tps",
    "reasoning_tps",
    "visible_tps",
)


def output_paths(output_dir: Path, session_id: str) -> tuple[Path, Path]:
    base = output_dir / f"codex-tps-{session_id}"
    return base.with_suffix(".csv"), base.with_suffix(".png")


def write_csv(path: Path, samples: Iterable[ResponseSample]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = _temporary_path(path)
    try:
        with temporary.open("w", encoding="utf-8", newline="") as stream:
            writer = csv.DictWriter(stream, fieldnames=CSV_COLUMNS)
            writer.writeheader()
            for sample in samples:
                writer.writerow(_csv_row(sample))
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def write_chart(path: Path, result: AnalysisResult) -> None:
    _configure_matplotlib_cache()
    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.dates as mdates
    import matplotlib.pyplot as plt
    from matplotlib.lines import Line2D

    path.parent.mkdir(parents=True, exist_ok=True)
    samples = result.samples
    x_values = [sample.completed_at for sample in samples]
    visible = [sample.visible_tps for sample in samples]
    reasoning = [sample.reasoning_tps for sample in samples]
    total = [sample.effective_tps for sample in samples]
    trend = _rolling_median(samples, window=7)

    fig, axis = plt.subplots(figsize=(16, 9), dpi=100)
    visible_color = "#2A6FDB"
    reasoning_color = "#E58A2B"
    total_color = "#172033"
    trend_color = "#7A3E9D"

    axis.vlines(
        x_values,
        [0.0] * len(samples),
        visible,
        color=visible_color,
        linewidth=1.5,
        alpha=0.48,
    )
    axis.vlines(
        x_values,
        visible,
        total,
        color=reasoning_color,
        linewidth=1.5,
        alpha=0.58,
    )
    axis.scatter(
        x_values,
        total,
        s=15,
        color=total_color,
        alpha=0.78,
        linewidths=0,
        zorder=3,
    )
    axis.plot(
        x_values,
        trend,
        color=trend_color,
        linewidth=2.1,
        alpha=0.92,
        zorder=4,
    )

    locator = mdates.AutoDateLocator(minticks=4, maxticks=10)
    axis.xaxis.set_major_locator(locator)
    axis.xaxis.set_major_formatter(mdates.ConciseDateFormatter(locator, show_offset=True))
    axis.set_xlabel("Response completion time (UTC)", labelpad=12)
    axis.set_ylabel("Effective output tokens per second", labelpad=12)
    axis.set_ylim(bottom=0)
    axis.grid(axis="y", color="#D7DCE3", linewidth=0.8, alpha=0.7)
    axis.spines["top"].set_visible(False)
    axis.spines["right"].set_visible(False)

    models = sorted({sample.model for sample in samples if sample.model})
    model_text = ", ".join(models) if models else "unknown model"
    median_tps = median(total)
    axis.set_title(
        "Codex effective output speed\n"
        f"{result.session_id}  ·  {model_text}  ·  "
        f"{len(samples):,} responses  ·  median {median_tps:.1f} TPS\n"
        f"Codex home: {result.codex_home}",
        loc="left",
        fontsize=15,
        fontweight="bold",
        pad=18,
    )

    legend = [
        Line2D([0], [0], color=visible_color, lw=4, label="Visible output TPS"),
        Line2D([0], [0], color=reasoning_color, lw=4, label="Reasoning TPS"),
        Line2D(
            [0],
            [0],
            marker="o",
            color="none",
            markerfacecolor=total_color,
            markeredgewidth=0,
            markersize=6,
            label="Total output TPS",
        ),
        Line2D([0], [0], color=trend_color, lw=2.1, label="7-response median"),
    ]
    axis.legend(handles=legend, loc="upper right", frameon=False, ncol=2)
    fig.text(
        0.01,
        0.012,
        "Effective TPS = output tokens ÷ model-response elapsed time. "
        "Tool runtime is excluded; first-token latency is included.",
        fontsize=10,
        color="#4C5566",
    )
    fig.tight_layout(rect=(0, 0.045, 1, 1))

    temporary = _temporary_path(path)
    try:
        fig.savefig(temporary, format="png", dpi=100, metadata={"Software": "codex-tps"})
        os.replace(temporary, path)
    finally:
        plt.close(fig)
        temporary.unlink(missing_ok=True)


def _rolling_median(
    samples: tuple[ResponseSample, ...], window: int
) -> list[float]:
    """Return a trailing median, breaking the line across five-minute gaps."""

    values: deque[float] = deque(maxlen=window)
    result: list[float] = []
    previous: datetime | None = None
    for sample in samples:
        if previous is not None and (sample.completed_at - previous).total_seconds() > 300:
            values.clear()
            values.append(sample.effective_tps)
            result.append(float("nan"))
        else:
            values.append(sample.effective_tps)
            result.append(median(values))
        previous = sample.completed_at
    return result


def _csv_row(sample: ResponseSample) -> dict[str, object]:
    return {
        "session_id": sample.session_id,
        "turn_id": sample.turn_id,
        "response_index": sample.response_index,
        "turn_response_index": sample.turn_response_index,
        "model": sample.model,
        "reasoning_effort": sample.reasoning_effort,
        "started_at": _format_timestamp(sample.started_at),
        "completed_at": _format_timestamp(sample.completed_at),
        "usage_recorded_at": _format_timestamp(sample.usage_recorded_at),
        "elapsed_seconds": f"{sample.elapsed_seconds:.6f}",
        "input_tokens": sample.input_tokens,
        "cached_input_tokens": sample.cached_input_tokens,
        "cache_write_input_tokens": sample.cache_write_input_tokens,
        "output_tokens": sample.output_tokens,
        "reasoning_output_tokens": sample.reasoning_output_tokens,
        "visible_output_tokens": sample.visible_output_tokens,
        "total_tokens": sample.total_tokens,
        "effective_tps": f"{sample.effective_tps:.6f}",
        "reasoning_tps": f"{sample.reasoning_tps:.6f}",
        "visible_tps": f"{sample.visible_tps:.6f}",
    }


def _format_timestamp(value: datetime) -> str:
    return value.isoformat(timespec="milliseconds").replace("+00:00", "Z")


def _temporary_path(destination: Path) -> Path:
    descriptor, name = tempfile.mkstemp(
        prefix=f".{destination.name}.", suffix=".tmp", dir=destination.parent
    )
    os.close(descriptor)
    return Path(name)


def _configure_matplotlib_cache() -> None:
    """Give Matplotlib a writable cache even in a restricted CLI environment."""

    if "MPLCONFIGDIR" in os.environ:
        return
    preferred = Path.home() / ".cache" / "codex-tps" / "matplotlib"
    try:
        preferred.mkdir(parents=True, exist_ok=True, mode=0o700)
        os.environ["MPLCONFIGDIR"] = str(preferred)
        return
    except OSError:
        pass

    user_id = os.getuid() if hasattr(os, "getuid") else "user"
    fallback = Path(tempfile.gettempdir()) / f"codex-tps-matplotlib-{user_id}"
    fallback.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.environ["MPLCONFIGDIR"] = str(fallback)
