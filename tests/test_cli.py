from __future__ import annotations

import csv
from pathlib import Path

from codex_tps.cli import main
from test_analysis import SESSION_ID, sample_records, write_rollout


def test_cli_writes_csv_and_png(tmp_path: Path, capsys) -> None:
    codex_home = tmp_path / "codex-home"
    output_dir = tmp_path / "reports"
    rollout = (
        codex_home
        / "sessions"
        / "2026"
        / "08"
        / "13"
        / f"rollout-{SESSION_ID}.jsonl"
    )
    write_rollout(rollout, sample_records())

    exit_code = main(
        [
            SESSION_ID,
            "--codex-home",
            str(codex_home),
            "--output-dir",
            str(output_dir),
        ]
    )

    assert exit_code == 0
    csv_path = output_dir / f"codex-tps-{SESSION_ID}.csv"
    png_path = output_dir / f"codex-tps-{SESSION_ID}.png"
    assert csv_path.is_file()
    assert png_path.read_bytes().startswith(b"\x89PNG\r\n\x1a\n")
    assert png_path.stat().st_size > 10_000

    with csv_path.open(newline="", encoding="utf-8") as stream:
        rows = list(csv.DictReader(stream))
    assert len(rows) == 2
    assert rows[0]["effective_tps"] == "25.000000"
    assert rows[0]["visible_output_tokens"] == "60"

    output = capsys.readouterr().out
    assert "Analyzed 2 model responses" in output
    assert "Median effective TPS: 22.5" in output

