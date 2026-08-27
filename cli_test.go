package codextps

import (
	"bytes"
	"encoding/csv"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIWritesCompatibleCSVAndPNG(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "codex-home")
	output := filepath.Join(root, "reports")
	rollout := filepath.Join(home, "sessions", "2026", "08", "13", "rollout-"+testSessionID+".jsonl")
	writeRollout(t, rollout, sampleRecords())
	var stdout, stderr bytes.Buffer
	code := Run([]string{testSessionID, "--codex-home", home, "--output-dir", output}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	csvPath := filepath.Join(output, "codex-tps-"+testSessionID+".csv")
	stream, err := os.Open(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(stream).ReadAll()
	_ = stream.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[1][17] != "25.000000" || rows[1][15] != "60" {
		t.Fatalf("rows=%v", rows)
	}
	pngPath := filepath.Join(output, "codex-tps-"+testSessionID+".png")
	imageFile, err := os.Open(pngPath)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := png.DecodeConfig(imageFile)
	_ = imageFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Width != 1600 || configuration.Height != 900 {
		t.Fatalf("dimensions=%dx%d", configuration.Width, configuration.Height)
	}
	if !strings.Contains(stdout.String(), "Analyzed 2 model responses") || !strings.Contains(stdout.String(), "Median effective TPS: 22.5") {
		t.Fatalf("stdout=%s", stdout.String())
	}
}
func TestVersionAndArgumentErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--version"}, &stdout, &stderr); code != 0 || stdout.String() != "codex-tps 0.2.0\n" {
		t.Fatalf("code=%d out=%q", code, stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run(nil, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "session_id") {
		t.Fatalf("code=%d err=%q", code, stderr.String())
	}
}
