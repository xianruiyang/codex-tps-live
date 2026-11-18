package codextps

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	liveNodeDefaultDownloadURL  = "https://librespeed.a573.net/backend/garbage.php?ckSize=8"
	liveNodeDefaultUploadURL    = "https://librespeed.a573.net/backend/empty.php"
	liveNodeDefaultBandwidthMiB = 8
)

func setLiveNodeBandwidthDefaults(options *liveNodeOptions) {
	hasBandwidth := false
	for _, test := range options.Tests {
		hasBandwidth = hasBandwidth || liveNodeBandwidth(test)
	}
	if !hasBandwidth {
		return
	}
	if options.BandwidthSeconds == 0 {
		options.BandwidthSeconds = 10
	}
	if options.BandwidthMiB == 0 {
		options.BandwidthMiB = liveNodeDefaultBandwidthMiB
	}
	if options.DownloadURL == "" || options.DownloadURL == "https://speed.cloudflare.com/__down?bytes=1000000000" ||
		options.DownloadURL == "https://speed.cloudflare.com/__down?bytes=25000000" {
		options.DownloadURL = liveNodeDefaultDownloadURL
	}
	if options.UploadURL == "" || options.UploadURL == "https://speed.cloudflare.com/__up" {
		options.UploadURL = liveNodeDefaultUploadURL
	}
}

func liveNodeBandwidth(endpoint string) bool {
	return endpoint == "download" || endpoint == "upload"
}

func liveNodeUploadTarget(raw string, size int64) (string, error) {
	target, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if target.Hostname() == "speed.cloudflare.com" && target.Path == "/__up" {
		query := target.Query()
		query.Set("bytes", fmt.Sprint(size))
		target.RawQuery = query.Encode()
	}
	return target.String(), nil
}

// Generate an exact-length upload batch in bounded memory. Each successful
// response acknowledges a complete request on the same connection.
type liveBandwidthUpload struct {
	mu      sync.Mutex
	buffer  []byte
	started time.Time
	bytes   int64
	limit   int64
}

func (s *liveBandwidthUpload) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started.IsZero() {
		s.started = time.Now()
	}
	if s.limit > 0 && s.bytes >= s.limit {
		return 0, io.EOF
	}
	if s.limit > 0 && int64(len(p)) > s.limit-s.bytes {
		p = p[:int(s.limit-s.bytes)]
	}
	n := copy(p, s.buffer)
	s.bytes += int64(n)
	return n, nil
}

func (s *liveBandwidthUpload) snapshot() (time.Time, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started, s.bytes
}

func probeLiveNodeBandwidth(ctx context.Context, port int, node, endpoint string, round int, options liveNodeOptions) (result liveNodeResult) {
	row := liveNodeResult{Node: node, Endpoint: endpoint, Round: round, Started: time.Now(), Outcome: "bandwidth_error", Exit: -1}
	var preflightStarted time.Time
	defer func() {
		if row.Preflight == nil {
			return
		}
		if !row.Preflight.Passed && row.Preflight.Seconds == 0 {
			row.Preflight.Seconds = time.Since(preflightStarted).Seconds()
		}
		result.Preflight = row.Preflight
		if row.Preflight.Passed {
			result.Note = "4 KiB 微量预检通过；" + result.Note
		} else {
			result.Note = "微量预检失败，未进行正式测速：" + result.Note
		}
	}()
	duration := time.Duration(options.BandwidthSeconds) * time.Second
	mib := options.BandwidthMiB
	if mib == 0 {
		mib = liveNodeDefaultBandwidthMiB
	}
	maxBytes := int64(mib) << 20
	timeout := time.Duration(options.Timeout) * time.Second
	preflightTimeout := min(timeout, 5*time.Second)
	requestCtx, cancel := context.WithTimeout(ctx, duration+2*timeout+preflightTimeout)
	defer cancel()
	proxy, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	transport := &http.Transport{
		Proxy:                 http.ProxyURL(proxy),
		DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
		DisableCompression:    true,
		MaxConnsPerHost:       1,
		// Match the single HTTP/1.1 connection used by the other node probes.
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	defer transport.CloseIdleConnections()

	var dials atomic.Int32
	if endpoint == "upload" {
		transport.DisableKeepAlives = false
		dialer := &net.Dialer{Timeout: timeout}
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if dials.Add(1) > 1 {
				return nil, errors.New("测速端已关闭单连接，未自动重连")
			}
			return dialer.DialContext(ctx, network, address)
		}
	}
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var phaseMu sync.Mutex
	phase := "连接节点代理"
	setPhase := func(value string) { phaseMu.Lock(); phase = value; phaseMu.Unlock() }
	written := make(chan error, 1)
	requestCtx = httptrace.WithClientTrace(requestCtx, &httptrace.ClientTrace{
		TLSHandshakeStart: func() { setPhase("TLS 握手") },
		GotConn:           func(httptrace.GotConnInfo) { setPhase("发送请求") },
		WroteHeaders: func() {
			if endpoint == "upload" {
				setPhase("发送上传数据")
			}
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				setPhase("等待接收端响应")
			}
			if endpoint == "upload" {
				written <- info.Err
			}
		},
		GotFirstResponseByte: func() { setPhase("读取响应头") },
	})
	var upload *liveBandwidthUpload
	failed := func(err error) liveNodeResult {
		row.Total = time.Since(row.Started).Seconds() * 1000
		if requestCtx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			row.Outcome = "timeout"
		}
		var requestError *url.Error
		if errors.As(err, &requestError) {
			err = requestError.Err
		}
		phaseMu.Lock()
		if row.Outcome != "timeout" && phase == "TLS 握手" {
			row.Outcome = "tls_error"
		}
		row.Note = phase + "阶段失败：" + err.Error()
		phaseMu.Unlock()
		if upload != nil {
			_, queued := upload.snapshot()
			row.Note += fmt.Sprintf("；本批已交给连接 %.2f KiB，接收端尚未确认", float64(queued)/1024)
		}
		return row
	}
	httpFailure := func(response *http.Response) liveNodeResult {
		row.Status, row.Total = response.StatusCode, time.Since(row.Started).Seconds()*1000
		row.Outcome = fmt.Sprintf("http_%d", response.StatusCode)
		row.Note = "测速地址未接受请求"
		if contentType := response.Header.Get("Content-Type"); strings.HasPrefix(contentType, "text/plain") || strings.HasPrefix(contentType, "application/json") {
			detail, _ := io.ReadAll(io.LimitReader(response.Body, 512))
			if text := strings.Join(strings.Fields(string(detail)), " "); text != "" {
				row.Note += " · " + text
			}
		}
		return row
	}
	// Exercise the selected endpoint with a tiny real request before sending
	// the measurement payload. Close it so the timed transfer has one fresh
	// connection and excludes validation time and data.
	preflightStarted = time.Now()
	row.Preflight = &liveNodePreflight{}
	preflightCtx, preflightCancel := context.WithTimeout(requestCtx, preflightTimeout)
	defer preflightCancel()
	method, target := http.MethodGet, options.DownloadURL
	body := bytes.NewReader(nil)
	if endpoint == "upload" {
		method = http.MethodPost
		var err error
		target, err = liveNodeUploadTarget(options.UploadURL, 4096)
		if err != nil {
			return failed(err)
		}
		payload := make([]byte, 4096)
		if _, err := rand.Read(payload); err != nil {
			return failed(err)
		}
		body = bytes.NewReader(payload)
	}
	preflightRequest, err := http.NewRequestWithContext(preflightCtx, method, target, body)
	if err != nil {
		return failed(err)
	}
	preflightRequest.Header.Set("Accept-Encoding", "identity")
	preflightRequest.Header.Set("Cache-Control", "no-cache")
	if endpoint == "download" {
		preflightRequest.Header.Set("Range", "bytes=0-4095")
	} else {
		preflightRequest.Header.Set("Content-Type", "application/octet-stream")
	}
	preflightResponse, err := client.Do(preflightRequest)
	if err != nil {
		return failed(err)
	}
	row.Preflight.Status = preflightResponse.StatusCode
	if preflightResponse.StatusCode < 200 || preflightResponse.StatusCode >= 300 {
		rejected := httpFailure(preflightResponse)
		preflightResponse.Body.Close()
		return rejected
	}
	read, err := io.Copy(io.Discard, io.LimitReader(preflightResponse.Body, 4096))
	preflightResponse.Body.Close()
	if err != nil {
		return failed(err)
	}
	if endpoint == "download" {
		row.Preflight.Bytes = read
		if read != 4096 {
			return failed(errors.New("下载地址未返回完整的 4 KiB 验证数据"))
		}
	} else {
		select {
		case err = <-written:
			if err != nil {
				return failed(err)
			}
		case <-preflightCtx.Done():
			return failed(preflightCtx.Err())
		}
		if body.Len() != 0 {
			return failed(errors.New("4 KiB 上传验证没有完整发送"))
		}
		row.Preflight.Bytes = 4096
	}
	row.Preflight.Passed = true
	row.Preflight.Seconds = time.Since(preflightStarted).Seconds()
	preflightCancel()
	transport.CloseIdleConnections()
	dials.Store(0)
	setPhase("连接节点代理")
	row.Status = 0
	var start, end time.Time
	if endpoint == "upload" {
		buffer := make([]byte, 32<<10)
		if _, err := rand.Read(buffer); err != nil {
			row.Outcome = "local_random_error"
			return row
		}
		batch := min(maxBytes, 4<<20)
		for {
			upload = &liveBandwidthUpload{buffer: buffer, limit: batch}
			target, err := liveNodeUploadTarget(options.UploadURL, batch)
			if err != nil {
				return failed(err)
			}
			req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, target, io.NopCloser(upload))
			if err != nil {
				return failed(err)
			}
			req.ContentLength = batch
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("Accept-Encoding", "identity")
			response, err := client.Do(req)
			if err != nil {
				return failed(err)
			}
			row.Status = response.StatusCode
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				result := httpFailure(response)
				response.Body.Close()
				return result
			}
			_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
			response.Body.Close()
			if err != nil {
				return failed(err)
			}
			select {
			case err = <-written:
				if err != nil {
					return failed(err)
				}
			case <-requestCtx.Done():
				return failed(requestCtx.Err())
			}
			sentAt, sent := upload.snapshot()
			if sent != batch {
				return failed(errors.New("上传批次没有完整发送"))
			}
			if start.IsZero() {
				start = sentAt
			}
			end = time.Now()
			row.TransferredBytes += sent
			remaining := duration - end.Sub(start)
			if remaining <= 100*time.Millisecond || row.TransferredBytes >= maxBytes || response.Close {
				if response.Close {
					row.Note = "测速端在已确认的请求后关闭了单连接"
				}
				break
			}
			// Grow acknowledged fixed-length batches while leaving time for a
			// final response. All requests reuse the same guarded connection.
			batchSeconds := max(time.Microsecond.Seconds(), end.Sub(sentAt).Seconds())
			rate := float64(batch) / batchSeconds
			budget := int64(rate * remaining.Seconds() * .7)
			if budget < 4096 {
				break
			}
			batch = min(batch*2, 4<<20, budget, maxBytes-row.TransferredBytes)
		}
		row.Note += "；单连接定长 POST，按已确认字节计算，含接收端响应等待"
	} else {
		target := options.DownloadURL
		if parsed, err := url.Parse(target); err == nil && parsed.Hostname() == "librespeed.a573.net" && parsed.Path == "/backend/garbage.php" {
			query := parsed.Query()
			query.Set("ckSize", fmt.Sprint(max(4, mib)))
			parsed.RawQuery = query.Encode()
			target = parsed.String()
		}
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, target, nil)
		if err != nil {
			return failed(err)
		}
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("Cache-Control", "no-cache")
		response, err := client.Do(req)
		if err != nil {
			return failed(err)
		}
		defer response.Body.Close()
		row.Status = response.StatusCode
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return httpFailure(response)
		}
		setPhase("下载响应体")
		buffer := make([]byte, 32<<10)
		start = time.Now()
		timer := time.AfterFunc(duration, cancel)
		row.TransferredBytes, err = io.CopyBuffer(io.Discard, io.LimitReader(response.Body, maxBytes), buffer)
		timer.Stop()
		end = time.Now()
		if err != nil && !(ctx.Err() == nil && requestCtx.Err() != nil && end.Sub(start) >= duration) {
			return failed(err)
		}
		row.Note = "单连接下载有效载荷速率；不含建连及响应头等待"
	}
	row.Total = time.Since(row.Started).Seconds() * 1000
	row.TransferSeconds = end.Sub(start).Seconds()
	if ctx.Err() != nil || row.TransferredBytes <= 0 || row.TransferSeconds <= 0 {
		row.Note = "传输中断或样本过短，未计算带宽"
		return row
	}
	speed := float64(row.TransferredBytes) * 8 / row.TransferSeconds / 1e6
	row.BandwidthMbps, row.Outcome, row.Exit = &speed, "bandwidth_ok", 0
	row.Note = strings.TrimPrefix(row.Note, "；")
	if row.TransferredBytes >= maxBytes {
		row.Note += fmt.Sprintf("；已达 %d MiB 流量上限", mib)
	}
	if row.TransferSeconds < 1 {
		row.BandwidthShort = true
		row.Note += "；传输样本不足 1 秒，瞬时波动影响较大，可提高流量上限"
	}
	if row.TransferSeconds < duration.Seconds()*.95 {
		row.Note += "；提前结束，按实际确认时长计算"
	}
	return row
}
