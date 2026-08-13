"""Command-line interface for codex-tps."""

from __future__ import annotations

import argparse
from pathlib import Path
from statistics import median
import sys
from typing import Sequence

from codex_tps import __version__
from codex_tps.analysis import (
    CodexTpsError,
    analyze_rollout,
    find_rollout,
    resolve_codex_home,
)
from codex_tps.report import output_paths, write_chart, write_csv


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="codex-tps",
        description=(
            "Export per-response effective tokens-per-second data from a Codex session."
        ),
    )
    parser.add_argument("session_id", help="Codex session UUID")
    parser.add_argument(
        "--codex-home",
        type=Path,
        help="Codex data directory (default: CODEX_HOME or ~/.codex)",
    )
    parser.add_argument(
        "--output-dir",
        type=Path,
        default=Path.cwd(),
        help="directory for the CSV and PNG (default: current directory)",
    )
    parser.add_argument("--version", action="version", version=f"%(prog)s {__version__}")
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    arguments = parser.parse_args(argv)

    try:
        codex_home = resolve_codex_home(arguments.codex_home)
        rollout = find_rollout(codex_home, arguments.session_id)
        result = analyze_rollout(rollout, arguments.session_id, codex_home)

        output_dir = arguments.output_dir.expanduser().resolve()
        csv_path, chart_path = output_paths(output_dir, result.session_id)
        write_csv(csv_path, result.samples)
        write_chart(chart_path, result)
    except (CodexTpsError, OSError) as error:
        parser.error(str(error))

    for warning in result.warnings:
        print(f"warning: {warning}", file=sys.stderr)

    median_tps = median(sample.effective_tps for sample in result.samples)
    print(f"Analyzed {len(result.samples):,} model responses from {result.session_id}")
    print(f"Median effective TPS: {median_tps:.1f}")
    print(f"CSV:   {csv_path}")
    print(f"Chart: {chart_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
