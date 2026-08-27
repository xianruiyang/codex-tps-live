// Package codextps analyzes local Codex rollout files and writes TPS reports.
package codextps

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const Version = "0.2.0"

type options struct {
	sessionID, codexHome, outputDir string
	version, help                   bool
}

// Run executes the codex-tps command and returns its process exit code.
func Run(arguments []string, stdout, stderr io.Writer) int {
	options, err := parseArguments(arguments)
	if err != nil {
		usage(stderr)
		fmt.Fprintf(stderr, "codex-tps: error: %v\n", err)
		return 2
	}
	if options.help {
		usage(stdout)
		return 0
	}
	if options.version {
		fmt.Fprintf(stdout, "codex-tps %s\n", Version)
		return 0
	}
	codexHome, err := resolveCodexHome(options.codexHome)
	if err != nil {
		fmt.Fprintf(stderr, "codex-tps: error: %v\n", err)
		return 2
	}
	rollout, err := findRollout(codexHome, options.sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "codex-tps: error: %v\n", err)
		return 2
	}
	result, err := analyzeRollout(rollout, options.sessionID, codexHome)
	if err != nil {
		fmt.Fprintf(stderr, "codex-tps: error: %v\n", err)
		return 2
	}
	outputDir := options.outputDir
	if outputDir == "" {
		outputDir, err = os.Getwd()
	} else {
		outputDir, err = absoluteExpanded(outputDir)
	}
	if err != nil {
		fmt.Fprintf(stderr, "codex-tps: error: %v\n", err)
		return 2
	}
	outputDir = filepath.Clean(outputDir)
	csvPath, chartPath := outputPaths(outputDir, result.SessionID)
	if err = writeCSV(csvPath, result.Samples); err == nil {
		err = writeChart(chartPath, result)
	}
	if err != nil {
		fmt.Fprintf(stderr, "codex-tps: error: %v\n", err)
		return 2
	}
	for _, warning := range result.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", warning)
	}
	values := make([]float64, len(result.Samples))
	for index, sample := range result.Samples {
		values[index] = sample.EffectiveTPS()
	}
	fmt.Fprintf(stdout, "Analyzed %d model responses from %s\nMedian effective TPS: %.1f\nCSV:   %s\nChart: %s\n", len(result.Samples), result.SessionID, median(values), csvPath, chartPath)
	return 0
}

func parseArguments(arguments []string) (options, error) {
	var result options
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "-h" || argument == "--help":
			result.help = true
		case argument == "--version":
			result.version = true
		case argument == "--codex-home" || argument == "--output-dir":
			if index+1 >= len(arguments) {
				return result, fmt.Errorf("argument %s: expected one value", argument)
			}
			index++
			if argument == "--codex-home" {
				result.codexHome = arguments[index]
			} else {
				result.outputDir = arguments[index]
			}
		case strings.HasPrefix(argument, "--codex-home="):
			result.codexHome = strings.TrimPrefix(argument, "--codex-home=")
		case strings.HasPrefix(argument, "--output-dir="):
			result.outputDir = strings.TrimPrefix(argument, "--output-dir=")
		case strings.HasPrefix(argument, "-"):
			return result, fmt.Errorf("unrecognized argument: %s", argument)
		default:
			if result.sessionID != "" {
				return result, fmt.Errorf("unexpected argument: %s", argument)
			}
			result.sessionID = argument
		}
	}
	if result.help || result.version {
		return result, nil
	}
	if result.sessionID == "" {
		return result, fmt.Errorf("the following argument is required: session_id")
	}
	return result, nil
}
func usage(writer io.Writer) {
	fmt.Fprintln(writer, "usage: codex-tps SESSION_ID [--codex-home PATH] [--output-dir PATH]\n\nExport per-response effective tokens-per-second data from a Codex session.")
}
