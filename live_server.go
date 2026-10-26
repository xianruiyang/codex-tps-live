package codextps

import (
	"context"
	"embed"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

//go:embed web/index.html web/trends.html web/quota.html web/nodes.html web/node_plans.html
var liveAssets embed.FS

const liveVersion = "0.20.2"

// RunLive starts a loopback-only dashboard. Codex logs remain read-only. Only
// authorized node tests and their saved continuations make outbound probes.
func RunLive(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("codex-tps-live", flag.ContinueOnError)
	flags.SetOutput(stderr)
	home := flags.String("codex-home", "", "Codex home (default: CODEX_HOME or ~/.codex)")
	port := flags.Int("port", 32555, "loopback port; 0 chooses an available port")
	days := flags.Int("days", 7, "discover logs modified in the last N days; 0 includes all history")
	interval := flags.Duration("interval", 500*time.Millisecond, "local tail polling interval (minimum 200ms)")
	archived := flags.Bool("include-archived", false, "show archived conversations by default (archived logs are always collected)")
	dataDir := flags.String("data-dir", "", "local history and test-progress directory (default: user local application data/CodexTPSLive)")
	quotaProfile := flags.String("quota-profile", "default", "local quota history namespace; does not identify a signed-in account")
	open := flags.Bool("open", false, "open the local dashboard in the default browser")
	clashHome := flags.String("clash-home", "", "Clash Verge data directory (default: discover in current user's application data)")
	mihomoPath := flags.String("mihomo-path", "", "Mihomo executable (default: discover beside running Clash Verge)")
	version := flags.Bool("version", false, "print version")
	if err := flags.Parse(arguments); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *version {
		fmt.Fprintf(stdout, "codex-tps-live %s (based on codex-tps v0.2.0)\n", liveVersion)
		return 0
	}
	if flags.NArg() != 0 || *port < 0 || *port > 65535 || *days < 0 || *interval < 200*time.Millisecond {
		fmt.Fprintln(stderr, "invalid arguments: port 0..65535, days >= 0, interval >= 200ms")
		return 2
	}
	resolved, err := resolveCodexHome(*home)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fmt.Fprintf(stderr, "Cannot listen on loopback: %v (use --port 0 for a free port)\n", err)
		return 2
	}
	address := listener.Addr().String()
	url := "http://" + address
	monitor := newLiveMonitor(resolved, *days, *archived, *interval)
	monitor.timingReader = &liveTimingReader{path: filepath.Join(resolved, "logs_2.sqlite")}
	defer monitor.timingReader.close()
	monitor.quota = openLiveQuotaHistory(resolved, *dataDir, *quotaProfile)
	defer func() {
		monitor.mu.Lock()
		defer monitor.mu.Unlock()
		monitor.quota.flush(true)
	}()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	nodes := &liveNodeService{ctx: ctx, home: *clashHome, binary: *mihomoPath}
	plans := openNodePlans(nodes, *dataDir)
	defer func() { stop(); nodes.close(); plans.close() }()
	go func() {
		monitor.poll()
		ticker := time.NewTicker(*interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				monitor.poll()
			}
		}
	}()
	mux := http.NewServeMux()
	nodes.register(mux)
	plans.register(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		page, _ := liveAssets.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeLiveJSON(w, monitor.snapshot(liveTurnWindow(r), quotaRules(r)))
	})
	mux.HandleFunc("/trends", func(w http.ResponseWriter, r *http.Request) {
		page, _ := liveAssets.ReadFile("web/trends.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
	mux.HandleFunc("/api/trends", func(w http.ResponseWriter, r *http.Request) {
		from, to, err := liveTrendRange(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeLiveJSON(w, monitor.trends(from, to, r.URL.Query().Get("view")))
	})
	mux.HandleFunc("/quota", func(w http.ResponseWriter, r *http.Request) {
		page, _ := liveAssets.ReadFile("web/quota.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
	mux.HandleFunc("/api/quota", func(w http.ResponseWriter, r *http.Request) {
		from, to, err := liveTrendRange(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeLiveJSON(w, monitor.quotaData(r, from, to))
	})
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		offset := queryInt(r, "offset", 0, 1000000)
		detail, ok := monitor.detail(r.URL.Query().Get("id"), offset, 50, liveTurnWindow(r), quotaRules(r))
		if !ok {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		writeLiveJSON(w, detail)
	})
	mux.HandleFunc("/api/responses", func(w http.ResponseWriter, r *http.Request) {
		rows, ok := monitor.responses(r.URL.Query().Get("id"), r.URL.Query().Get("turn"), quotaRules(r))
		if !ok {
			http.Error(w, "turn not found", http.StatusNotFound)
			return
		}
		writeLiveJSON(w, rows)
	})
	mux.HandleFunc("/api/export", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		rows, ok := monitor.allResponses(id, liveTurnWindow(r))
		if !ok {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "codex-tps-" + id + ".csv"}))
		_, _ = w.Write([]byte{0xef, 0xbb, 0xbf})
		writeLiveCSV(w, id, rows)
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Loopback bind alone cannot stop DNS rebinding or cross-origin browser reads.
		if r.Host != address || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != url) {
			http.Error(w, "local origin required", http.StatusForbidden)
			return
		}
		nodeAction := r.Method == http.MethodPost && (r.URL.Path == "/api/nodes/start" || r.URL.Path == "/api/nodes/cancel" || r.URL.Path == "/api/node-plans/save" || r.URL.Path == "/api/node-plans/action")
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !nodeAction {
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	fmt.Fprintf(stdout, "Codex TPS Live\nDashboard: %s\nRead-only log directory: %s\nPress Ctrl+C to stop. Node probes run on demand, by enabled plans, or by unfinished-job recovery; no model generation requests.\n", url, resolved)
	fmt.Fprintf(stdout, "Local quota history: %s\n", monitor.quota.description())
	if *open {
		if err := openLiveBrowser(url); err != nil {
			fmt.Fprintf(stderr, "Open %s in your browser (%v)\n", url, err)
		}
	}
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
func openLiveBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
func queryInt(r *http.Request, key string, fallback, maximum int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil || n < 0 {
		return fallback
	}
	return min(n, maximum)
}

func liveTurnWindow(r *http.Request) int {
	return max(1, queryInt(r, "turns", 10, 1000000))
}
func writeLiveJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}
func safeLiveCSV(s string) string {
	trimmed := strings.TrimLeft(s, " \t\r\n")
	if len(trimmed) > 0 && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + s
	}
	return s
}
func writeLiveCSV(stream io.Writer, id string, responses []liveResponse) {
	w := csv.NewWriter(stream)
	w.UseCRLF = true
	_ = w.Write([]string{"session_id", "turn_id", "response_index", "turn_response_index", "model", "reasoning_effort", "started_at", "completed_at", "usage_recorded_at", "elapsed_seconds", "input_tokens", "cached_input_tokens", "cache_write_input_tokens", "output_tokens", "reasoning_output_tokens", "visible_output_tokens", "effective_tps", "reasoning_tps", "visible_tps", "timing_note", "perceived_tps", "actual_tps", "ttft_seconds", "ttft_estimated", "ttft_source", "generation_seconds", "ttft_upper_bound", "transport", "transport_fallback", "transport_note", "transport_inferred", "actual_tps_min", "actual_tps_max", "actual_range", "generation_min_seconds", "generation_max_seconds", "ttft_assumed", "ttft_note", "transport_source"})
	for _, r := range responses {
		seconds := ""
		if r.Seconds > 0 {
			seconds = fmt.Sprintf("%.6f", r.Seconds)
		}
		first, estimated, source := "", "", ""
		upperBound, assumed, firstNote := "", "", ""
		if r.TTFT != nil {
			first, estimated, source = fmt.Sprintf("%.6f", r.TTFT.Seconds), strconv.FormatBool(r.TTFT.Estimated), r.TTFT.Source
			upperBound = strconv.FormatBool(r.TTFT.UpperBound)
			assumed, firstNote = strconv.FormatBool(r.TTFT.Assumed), r.TTFT.Note
		}
		generation := ""
		generationMinimum, generationMaximum := "", ""
		if r.Seconds > 0 {
			generationMinimum, generationMaximum = fmt.Sprintf("%.6f", r.GenerationMinSeconds), fmt.Sprintf("%.6f", r.GenerationMaxSeconds)
		}
		if r.GenerationSeconds > 0 {
			generation = fmt.Sprintf("%.6f", r.GenerationSeconds)
		}
		_ = w.Write([]string{safeLiveCSV(id), safeLiveCSV(r.TurnID), strconv.Itoa(r.Index), strconv.Itoa(r.TurnIndex), safeLiveCSV(r.Model), safeLiveCSV(r.Effort), liveTime(r.StartedAt), liveTime(r.CompletedAt), liveTime(r.RecordedAt), seconds, strconv.FormatInt(r.Tokens.Input, 10), strconv.FormatInt(r.Tokens.Cached, 10), strconv.FormatInt(r.Tokens.CacheWrite, 10), strconv.FormatInt(r.Tokens.Output, 10), strconv.FormatInt(r.Tokens.Reasoning, 10), strconv.FormatInt(r.Tokens.Visible, 10), liveDecimal(r.TPS.Output), liveDecimal(r.TPS.Reasoning), liveDecimal(r.TPS.Visible), safeLiveCSV(r.TimingNote), liveDecimal(r.TPS.Perceived), liveDecimal(r.TPS.Actual), first, estimated, source, generation, upperBound, r.Transport, strconv.FormatBool(r.TransportFallback), safeLiveCSV(r.TransportNote), strconv.FormatBool(r.TransportInferred), liveDecimal(r.TPS.ActualMin), liveDecimal(r.TPS.ActualMax), strconv.FormatBool(r.TPS.ActualRange), generationMinimum, generationMaximum, assumed, safeLiveCSV(firstNote), r.TransportSource})
	}
	w.Flush()
}
func liveTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}
func liveDecimal(v *float64) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%.6f", *v)
}
