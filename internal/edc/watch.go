package edc

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

type watchSample struct {
	Type         string    `json:"type"`
	Time         time.Time `json:"time"`
	Status       Status    `json:"status"`
	HTTPStatus   int       `json:"http_status,omitempty"`
	BodyBytes    int64     `json:"body_bytes"`
	DurationMS   int64     `json:"duration_ms"`
	PeerIP       string    `json:"peer_ip,omitempty"`
	DNSHost      string    `json:"dns_host,omitempty"`
	DNSAddresses []string  `json:"dns_addresses,omitempty"`
	DNSChanged   bool      `json:"dns_changed,omitempty"`
	DNSMS        *int64    `json:"dns_ms,omitempty"`
	TCPMS        *int64    `json:"tcp_ms,omitempty"`
	TLSMS        *int64    `json:"tls_ms,omitempty"`
	TTFBMS       *int64    `json:"ttfb_ms,omitempty"`
	Phase        string    `json:"phase,omitempty"`
	Error        string    `json:"error,omitempty"`
}

type watchSummary struct {
	Type             string `json:"type"`
	Samples          int    `json:"samples"`
	Pass             int    `json:"pass"`
	Warn             int    `json:"warn"`
	Fail             int    `json:"fail"`
	DurationMS       int64  `json:"duration_ms"`
	MinMS            int64  `json:"min_ms"`
	AvgMS            int64  `json:"avg_ms"`
	P95MS            int64  `json:"p95_ms"`
	MaxMS            int64  `json:"max_ms"`
	LongestFailureMS int64  `json:"longest_failure_ms"`
}

func runWatch(args []string) int {
	options := configuredCommon(15 * time.Second)
	set := flag.NewFlagSet("watch", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	intervalText := "1"
	set.StringVar(&intervalText, "i", intervalText, T("command.watch.option.interval"))
	set.StringVar(&intervalText, "interval", intervalText, T("command.watch.option.interval"))
	duration := set.Duration("duration", 0, T("command.watch.option.duration"))
	expectStatus := set.Int("expect-status", configuredInt(activeConfig.Defaults.HTTP.ExpectStatus, 0), T("command.http.option.expect_status"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 1 {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc watch [-i seconds] [--duration 1m] [options] <host|URL>"))
		return 2
	}
	interval, err := parseObserveInterval(intervalText)
	if err != nil || *duration < 0 || (*expectStatus != 0 && (*expectStatus < 100 || *expectStatus > 599)) {
		if err == nil {
			err = fmt.Errorf("%s", T("observe.watch.options_invalid"))
		}
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	_, _, rawURL, err := normalizeTarget(set.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	writer, closeOutput, err := openObserveStream(options.jsonPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer closeOutput()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}
	return streamWatch(ctx, writer, interval, options.timeout, options.jsonPath != "", options.redact, func(ctx context.Context) Result {
		return probeHTTPWithOptions(ctx, rawURL, httpCheckOptions{expectStatus: *expectStatus})
	})
}

func streamWatch(ctx context.Context, writer io.Writer, interval, timeout time.Duration, jsonOutput, redact bool, probe func(context.Context) Result) int {
	started := time.Now()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	encoder := json.NewEncoder(writer)
	summary := watchSummary{Type: "summary"}
	latencyCounts := map[int64]int64{}
	var latencyTotal int64
	var failureStart time.Time
	var previousDNS string
	for ctx.Err() == nil {
		sampleCtx, cancel := context.WithTimeout(ctx, timeout)
		result := probe(sampleCtx)
		cancel()
		if ctx.Err() != nil {
			break
		}
		sample := watchSampleOf(result)
		if len(sample.DNSAddresses) > 0 {
			currentDNS := sample.DNSHost + "|" + strings.Join(sample.DNSAddresses, ",")
			sample.DNSChanged = currentDNS != previousDNS
			previousDNS = currentDNS
		}
		if redact {
			sample.PeerIP = redactIPAddresses(sample.PeerIP)
			sample.DNSHost = redactIPAddresses(sample.DNSHost)
			for index := range sample.DNSAddresses {
				sample.DNSAddresses[index] = redactIPAddresses(sample.DNSAddresses[index])
			}
			sample.Error = redactIPAddresses(sample.Error)
		}
		if jsonOutput {
			if err := encoder.Encode(sample); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
		} else {
			fmt.Fprintln(writer, formatWatchSample(sample))
		}
		summary.Samples++
		latencyCounts[sample.DurationMS]++
		latencyTotal += sample.DurationMS
		if summary.Samples == 1 || sample.DurationMS < summary.MinMS {
			summary.MinMS = sample.DurationMS
		}
		if sample.DurationMS > summary.MaxMS {
			summary.MaxMS = sample.DurationMS
		}
		switch sample.Status {
		case StatusPass:
			summary.Pass++
		case StatusWarn:
			summary.Warn++
		case StatusFail:
			summary.Fail++
			if failureStart.IsZero() {
				failureStart = sample.Time
			}
		}
		if sample.Status != StatusFail && !failureStart.IsZero() {
			if elapsed := sample.Time.Sub(failureStart).Milliseconds(); elapsed > summary.LongestFailureMS {
				summary.LongestFailureMS = elapsed
			}
			failureStart = time.Time{}
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	summary.DurationMS = time.Since(started).Milliseconds()
	if !failureStart.IsZero() {
		if elapsed := time.Since(failureStart).Milliseconds(); elapsed > summary.LongestFailureMS {
			summary.LongestFailureMS = elapsed
		}
	}
	if summary.Samples > 0 {
		summary.AvgMS = latencyTotal / int64(summary.Samples)
		values := make([]int64, 0, len(latencyCounts))
		for latency := range latencyCounts {
			values = append(values, latency)
		}
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		rank := int64((summary.Samples*95 + 99) / 100)
		var counted int64
		for _, latency := range values {
			counted += latencyCounts[latency]
			if counted >= rank {
				summary.P95MS = latency
				break
			}
		}
	}
	if jsonOutput {
		if err := encoder.Encode(summary); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	} else {
		fmt.Fprintln(writer, T("observe.watch.summary", summary.Samples, summary.Pass, summary.Warn, summary.Fail, time.Duration(summary.DurationMS)*time.Millisecond))
		fmt.Fprintln(writer, T("observe.watch.latency_summary", summary.MinMS, summary.AvgMS, summary.P95MS, summary.MaxMS, time.Duration(summary.LongestFailureMS)*time.Millisecond))
	}
	if summary.Fail > 0 {
		return 1
	}
	return 0
}

func watchSampleOf(result Result) watchSample {
	sample := watchSample{Type: "sample", Time: time.Now().UTC(), Status: result.Status, DurationMS: result.DurationMS}
	if code, ok := result.Metrics["status_code"].(int); ok {
		sample.HTTPStatus = code
	}
	if size, ok := result.Metrics["bytes_read"].(int64); ok {
		sample.BodyBytes = size
	}
	if peer, ok := result.Metrics["peer_ip"].(string); ok {
		sample.PeerIP = peer
	}
	if host, ok := result.Metrics["resolved_host"].(string); ok {
		sample.DNSHost = host
	}
	if addresses, ok := result.Metrics["resolved_addresses"].([]string); ok {
		sample.DNSAddresses = append([]string(nil), addresses...)
	}
	for _, phase := range []struct {
		key   string
		value **int64
	}{{"dns_ms", &sample.DNSMS}, {"connect_ms", &sample.TCPMS}, {"tls_ms", &sample.TLSMS}, {"ttfb_ms", &sample.TTFBMS}} {
		if value, ok := result.Metrics[phase.key].(int64); ok {
			copy := value
			*phase.value = &copy
		}
	}
	if result.Error != nil {
		sample.Phase = result.Error.Kind
		sample.Error = firstLine(result.Error.Message)
	} else if result.Status == StatusFail {
		sample.Error = firstLine(result.Summary)
	}
	return sample
}

func formatWatchSample(sample watchSample) string {
	http := "HTTP —"
	if sample.HTTPStatus != 0 {
		http = fmt.Sprintf("HTTP %d", sample.HTTPStatus)
	}
	parts := []string{sample.Time.Local().Format("15:04:05.000"), strings.ToUpper(string(sample.Status)), http, fmt.Sprintf("%d bytes", sample.BodyBytes), fmt.Sprintf("%dms", sample.DurationMS)}
	if sample.PeerIP != "" {
		parts = append(parts, "peer "+sample.PeerIP)
	}
	for _, phase := range []struct {
		name  string
		value *int64
	}{{"DNS", sample.DNSMS}, {"TCP", sample.TCPMS}, {"TLS", sample.TLSMS}, {"TTFB", sample.TTFBMS}} {
		if phase.value != nil {
			parts = append(parts, fmt.Sprintf("%s %dms", phase.name, *phase.value))
		}
	}
	if sample.DNSChanged {
		parts = append(parts, "DNS["+sample.DNSHost+"] "+strings.Join(sample.DNSAddresses, ","))
	}
	if sample.Phase != "" {
		parts = append(parts, "phase "+sample.Phase)
	}
	if sample.Error != "" {
		parts = append(parts, sample.Error)
	}
	return strings.Join(parts, "  ")
}
