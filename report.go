package codextps

import (
	"encoding/csv"
	"fmt"
	"image/color"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gonum.org/v1/plot"
	"gonum.org/v1/plot/plotter"
	"gonum.org/v1/plot/vg"
	"gonum.org/v1/plot/vg/draw"
	"gonum.org/v1/plot/vg/vgimg"
)

var csvColumns = []string{"session_id", "turn_id", "response_index", "turn_response_index", "model", "reasoning_effort", "started_at", "completed_at", "usage_recorded_at", "elapsed_seconds", "input_tokens", "cached_input_tokens", "cache_write_input_tokens", "output_tokens", "reasoning_output_tokens", "visible_output_tokens", "total_tokens", "effective_tps", "reasoning_tps", "visible_tps"}

func outputPaths(outputDir, sessionID string) (string, string) {
	base := filepath.Join(outputDir, "codex-tps-"+sessionID)
	return base + ".csv", base + ".png"
}

func writeCSV(path string, samples []ResponseSample) error {
	return atomicWrite(path, 0o600, func(stream io.Writer) error {
		writer := csv.NewWriter(stream)
		writer.UseCRLF = true
		if err := writer.Write(csvColumns); err != nil {
			return err
		}
		for _, sample := range samples {
			row := []string{sample.SessionID, sample.TurnID, strconv.Itoa(sample.ResponseIndex), strconv.Itoa(sample.TurnResponseIndex), sample.Model, sample.ReasoningEffort, formatTimestamp(sample.StartedAt), formatTimestamp(sample.CompletedAt), formatTimestamp(sample.UsageRecordedAt), fmt.Sprintf("%.6f", sample.ElapsedSeconds), strconv.FormatInt(sample.InputTokens, 10), strconv.FormatInt(sample.CachedInputTokens, 10), strconv.FormatInt(sample.CacheWriteInputTokens, 10), strconv.FormatInt(sample.OutputTokens, 10), strconv.FormatInt(sample.ReasoningOutputTokens, 10), strconv.FormatInt(sample.VisibleOutputTokens, 10), strconv.FormatInt(sample.TotalTokens, 10), fmt.Sprintf("%.6f", sample.EffectiveTPS()), fmt.Sprintf("%.6f", sample.ReasoningTPS()), fmt.Sprintf("%.6f", sample.VisibleTPS())}
			if err := writer.Write(row); err != nil {
				return err
			}
		}
		writer.Flush()
		return writer.Error()
	})
}

func writeChart(path string, result AnalysisResult) error {
	p := plot.New()
	p.X.Label.Text = "Response completion time (UTC)"
	p.Y.Label.Text = "Effective output tokens per second"
	p.Y.Min = 0
	p.X.Tick.Marker = plot.TimeTicks{Format: "2006-01-02\n15:04:05"}
	p.Add(plotter.NewGrid())
	models := map[string]struct{}{}
	values := make([]float64, 0, len(result.Samples))
	for _, sample := range result.Samples {
		if sample.Model != "" {
			models[sample.Model] = struct{}{}
		}
		values = append(values, sample.EffectiveTPS())
	}
	modelList := make([]string, 0, len(models))
	for model := range models {
		modelList = append(modelList, model)
	}
	sort.Strings(modelList)
	modelText := strings.Join(modelList, ", ")
	if modelText == "" {
		modelText = "unknown model"
	}
	p.Title.Text = fmt.Sprintf("Codex effective output speed\n%s · %s · %d responses · median %.1f TPS\nCodex home: %s", result.SessionID, modelText, len(result.Samples), median(values), result.CodexHome)

	visibleColor, reasoningColor := color.RGBA{42, 111, 219, 180}, color.RGBA{229, 138, 43, 190}
	for _, sample := range result.Samples {
		x := float64(sample.CompletedAt.UnixNano()) / 1e9
		visible, total := sample.VisibleTPS(), sample.EffectiveTPS()
		first, _ := plotter.NewLine(plotter.XYs{{X: x, Y: 0}, {X: x, Y: visible}})
		first.Color = visibleColor
		first.Width = vg.Points(1.5)
		p.Add(first)
		second, _ := plotter.NewLine(plotter.XYs{{X: x, Y: visible}, {X: x, Y: total}})
		second.Color = reasoningColor
		second.Width = vg.Points(1.5)
		p.Add(second)
	}
	points := make(plotter.XYs, len(result.Samples))
	for i, sample := range result.Samples {
		points[i].X = float64(sample.CompletedAt.UnixNano()) / 1e9
		points[i].Y = sample.EffectiveTPS()
	}
	scatter, err := plotter.NewScatter(points)
	if err != nil {
		return err
	}
	scatter.GlyphStyle.Color = color.RGBA{23, 32, 51, 220}
	scatter.GlyphStyle.Radius = vg.Points(2.5)
	p.Add(scatter)
	p.Legend.Add("Total output TPS", scatter)
	for _, segment := range trendSegments(result.Samples, 7) {
		if len(segment) < 2 {
			continue
		}
		line, err := plotter.NewLine(segment)
		if err != nil {
			return err
		}
		line.Color = color.RGBA{122, 62, 157, 235}
		line.Width = vg.Points(2.1)
		p.Add(line)
		p.Legend.Add("7-response median", line)
	}
	p.Legend.Top = true

	return atomicWrite(path, 0o600, func(stream io.Writer) error {
		canvas := vgimg.NewWith(vgimg.UseWH(16*vg.Inch, 9*vg.Inch), vgimg.UseDPI(100))
		p.Draw(draw.New(canvas))
		png := vgimg.PngCanvas{Canvas: canvas}
		_, err := png.WriteTo(stream)
		return err
	})
}

func trendSegments(samples []ResponseSample, window int) []plotter.XYs {
	var segments []plotter.XYs
	current := plotter.XYs{}
	history := []float64{}
	var previous time.Time
	for _, sample := range samples {
		if !previous.IsZero() && sample.CompletedAt.Sub(previous) > 5*time.Minute {
			if len(current) > 0 {
				segments = append(segments, current)
			}
			current = plotter.XYs{}
			history = []float64{sample.EffectiveTPS()}
			previous = sample.CompletedAt
			continue
		}
		history = append(history, sample.EffectiveTPS())
		if len(history) > window {
			history = history[len(history)-window:]
		}
		current = append(current, plotter.XY{X: float64(sample.CompletedAt.UnixNano()) / 1e9, Y: median(history)})
		previous = sample.CompletedAt
	}
	if len(current) > 0 {
		segments = append(segments, current)
	}
	return segments
}

func median(values []float64) float64 {
	copy := append([]float64(nil), values...)
	sort.Float64s(copy)
	if len(copy) == 0 {
		return math.NaN()
	}
	middle := len(copy) / 2
	if len(copy)%2 == 1 {
		return copy[middle]
	}
	return (copy[middle-1] + copy[middle]) / 2
}
func formatTimestamp(value time.Time) string { return value.Format("2006-01-02T15:04:05.000Z07:00") }
func atomicWrite(path string, mode os.FileMode, write func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err = write(temporary); err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(temporaryPath, mode)
	}
	if err == nil {
		err = os.Rename(temporaryPath, path)
	}
	ok = err == nil
	return err
}
