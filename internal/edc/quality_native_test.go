package edc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const qualityFloatTolerance = 1e-9

func qualityClose(got, want float64) bool { return math.Abs(got-want) <= qualityFloatTolerance }

func TestQualityNativeTrimmedMean(t *testing.T) {
	outlierSet := make([]float64, 0, 20)
	for value := 1; value <= 19; value++ {
		outlierSet = append(outlierSet, float64(value))
	}
	outlierSet = append(outlierSet, 1000)
	cases := []struct {
		name    string
		samples []float64
		tmp     float64
		want    float64
		ok      bool
	}{
		{"empty", nil, defaultTrimmedMeanPercentile, 0, false},
		{"one sample", []float64{7}, defaultTrimmedMeanPercentile, 7, true},
		{"two samples keep both", []float64{9, 3}, defaultTrimmedMeanPercentile, 6, true},
		{"drops the slowest five percent", outlierSet, defaultTrimmedMeanPercentile, 10, true},
		{"drops only the top side", []float64{4, 1, 3, 2}, 0.5, 1.5, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := trimmedMean(tc.samples, tc.tmp)
			if ok != tc.ok || !qualityClose(got, tc.want) {
				t.Fatalf("trimmedMean = %v, %v; want %v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestQualityNativeRPM(t *testing.T) {
	cases := []struct {
		name    string
		samples responsivenessSamples
		want    float64
		ok      bool
	}{
		{"tls foreign and loaded", responsivenessSamples{TCPForeign: []float64{10}, TLSForeign: []float64{20}, HTTPForeign: []float64{30}, HTTPLoaded: []float64{100}}, 1800, true},
		{"tcp only", responsivenessSamples{TCPForeign: []float64{10}, HTTPForeign: []float64{50}, HTTPLoaded: []float64{100}}, 1300, true},
		{"foreign only without self probes", responsivenessSamples{TCPForeign: []float64{10}, TLSForeign: []float64{20}, HTTPForeign: []float64{30}}, 3000, true},
		{"loaded only", responsivenessSamples{HTTPLoaded: []float64{200}}, 300, true},
		{"zero loaded time", responsivenessSamples{HTTPLoaded: []float64{0}}, 0, false},
		{"no samples", responsivenessSamples{}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := responsivenessRPM(tc.samples, defaultTrimmedMeanPercentile)
			if ok != tc.ok || !qualityClose(got, tc.want) {
				t.Fatalf("responsivenessRPM = %v, %v; want %v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestQualityNativeTLSNormalize(t *testing.T) {
	const handshake = 30 * time.Millisecond
	if got := normalizeTLSHandshake(handshake, tls.VersionTLS12); !qualityClose(got, 15) {
		t.Fatalf("TLS 1.2 = %v", got)
	}
	if got := normalizeTLSHandshake(handshake, tls.VersionTLS13); !qualityClose(got, 30) {
		t.Fatalf("TLS 1.3 = %v", got)
	}
}

func TestQualityNativeGoodput(t *testing.T) {
	second := time.Second
	cases := []struct {
		name      string
		intervals []goodputInterval
		want      float64
	}{
		{"empty", nil, 0},
		{"one interval", []goodputInterval{{Bytes: 1_000_000, Duration: second}}, 8_000_000},
		{"last MAD intervals only", []goodputInterval{{100, second}, {1000, second}, {1000, second}, {1000, second}, {1000, second}}, 8000},
		{"partial interval", []goodputInterval{{Bytes: 500, Duration: 500 * time.Millisecond}}, 8000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := movingGoodput(tc.intervals, defaultMovingAverageDistance); !qualityClose(got, tc.want) {
				t.Fatalf("movingGoodput = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestQualityNativeStability(t *testing.T) {
	cases := []struct {
		name   string
		values []float64
		want   bool
	}{
		{"flat", []float64{100, 100, 100, 100}, true},
		{"too few intervals", []float64{100, 100, 100}, false},
		{"swinging", []float64{90, 110, 90, 110}, false},
		{"only the last MAD values count", []float64{50, 100, 101, 99, 100}, true},
		{"zero current", []float64{0, 0, 0, 0}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stable(tc.values, defaultMovingAverageDistance, defaultStdDevTolerance); got != tc.want {
				t.Fatalf("stable(%v) = %v", tc.values, got)
			}
		})
	}
}

func TestQualityNativeConfidence(t *testing.T) {
	cases := []struct {
		intervals int
		stable    bool
		want      qualityConfidence
	}{
		{1, false, qualityConfidenceLow},
		{1, true, qualityConfidenceLow},
		{defaultMovingAverageDistance, false, qualityConfidenceMedium},
		{defaultMovingAverageDistance + 1, true, qualityConfidenceHigh},
	}
	for _, tc := range cases {
		if got := measurementConfidence(tc.intervals, defaultMovingAverageDistance, tc.stable); got != tc.want {
			t.Fatalf("confidence(%d, %v) = %s, want %s", tc.intervals, tc.stable, got, tc.want)
		}
	}
}

func TestQualityNativeProbeRate(t *testing.T) {
	params := defaultResponsivenessParams()
	cases := []struct {
		name    string
		goodput float64
		want    float64
	}{
		{"unknown goodput starts at the initial rate", 0, initialProbePairsPerSecond},
		{"slow link is capped by capacity share", 8_000_000, defaultProbeCapacityShare * 1_000_000 / (foreignProbeBytes + selfProbeBytes)},
		{"fast link is capped by MPS", 1_000_000_000, defaultMaxProbesPerSecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := probePairsPerSecond(params, tc.goodput); !qualityClose(got, tc.want) {
				t.Fatalf("rate = %v, want %v", got, tc.want)
			}
		})
	}
}

const qualityAppleConfigFixture = `{
  "version": 1,
  "test_endpoint": "edge-1.quality.example.net",
  "urls": {
    "small_https_download_url": "https://quality.example.net/api/v1/gm/small",
    "large_https_download_url": "https://quality.example.net/api/v1/gm/large",
    "https_upload_url": "https://quality.example.net/api/v1/gm/slurp",
    "small_download_url": "https://quality.example.net/api/v1/gm/small",
    "large_download_url": "https://quality.example.net/api/v1/gm/large",
    "upload_url": "https://quality.example.net/api/v1/gm/slurp"
  }
}`

func TestQualityNativeConfigFetch(t *testing.T) {
	oversized := `{"version": 1` + strings.Repeat(" ", configBodyLimit) + `}`
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"apple shape", http.StatusOK, qualityAppleConfigFixture, nil},
		{"draft names", http.StatusOK, `{"version":1,"urls":{"large_download_url":"http://h.example/large","small_download_url":"http://h.example/small","upload_url":"http://h.example/up"}}`, nil},
		{"server error", http.StatusInternalServerError, "", errQualityConfigStatus},
		{"bad json", http.StatusOK, `{"version":`, errQualityConfigJSON},
		{"wrong version", http.StatusOK, `{"version":2,"urls":{}}`, errQualityConfigVersion},
		{"missing urls", http.StatusOK, `{"version":1}`, errQualityConfigMissingURL},
		{"mixed hosts", http.StatusOK, `{"version":1,"urls":{"large_download_url":"https://a.example/large","small_download_url":"https://b.example/small","upload_url":"https://a.example/up"}}`, errQualityConfigMixedHosts},
		{"ftp scheme", http.StatusOK, `{"version":1,"urls":{"large_download_url":"ftp://a.example/large","small_download_url":"ftp://a.example/small","upload_url":"ftp://a.example/up"}}`, errQualityServerInvalid},
		{"oversized body", http.StatusOK, oversized, errQualityConfigTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(tc.status)
				_, _ = writer.Write([]byte(tc.body))
			}))
			defer server.Close()
			config, err := fetchResponsivenessConfig(context.Background(), server.Client(), server.URL+"/config")
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("error = %v, want %v", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(config.LargeDownloadURL, "/large") || !strings.HasSuffix(config.SmallDownloadURL, "/small") || config.UploadURL == "" {
				t.Fatalf("config = %+v", config)
			}
		})
	}
}

func TestQualityNativeConfigPrefersHTTPSNames(t *testing.T) {
	body := `{"version":1,"test_endpoint":"edge","urls":{"large_https_download_url":"https://h.example/large","large_download_url":"http://h.example/large","small_https_download_url":"https://h.example/small","https_upload_url":"https://h.example/up"}}`
	config, err := parseResponsivenessConfig(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if config.LargeDownloadURL != "https://h.example/large" || config.TestEndpoint != "edge" {
		t.Fatalf("config = %+v", config)
	}
}

func TestQualityNativeConfigServerValidation(t *testing.T) {
	cases := []struct {
		raw   string
		valid bool
	}{
		{"https://quality.example.net/api/v1/gm/config", true},
		{"http://127.0.0.1:8080/config", true},
		{"", false},
		{"relative/config", false},
		{"/absolute/path", false},
		{"file:///etc/config", false},
		{"https://", false},
		{"ftp://quality.example.net/config", false},
		{"https://user:secret@quality.example.net/config", false},
	}
	for _, tc := range cases {
		err := validateQualityServer(tc.raw)
		if (err == nil) != tc.valid {
			t.Fatalf("validateQualityServer(%q) = %v", tc.raw, err)
		}
		if err != nil && !errors.Is(err, errQualityServerInvalid) {
			t.Fatalf("validateQualityServer(%q) error type = %v", tc.raw, err)
		}
	}
}

const (
	qualityTestChunkBytes    = 32 << 10
	qualityTestFiniteChunks  = 8
	qualityTestUploadLimit   = 256 << 10
	qualityTestAbortAfter    = 64 << 10
	qualityTestCancelAfter   = 300 * time.Millisecond
	qualityTestRunTimeout    = 5 * time.Second
	qualityTestReturnSlack   = 500 * time.Millisecond
	qualityTestInterval      = 50 * time.Millisecond
	qualityTestMaxRun        = time.Second
	qualityTestGrace         = 500 * time.Millisecond
	qualityTestReserve       = 100 * time.Millisecond
	qualityTestMovingWindow  = 2
	qualityTestIdleProbes    = 2
	qualityTestProbeRate     = 40
	qualityTestInflightLimit = 8

	qualityTestShortDeadline = 400 * time.Millisecond
	qualityTestLongDeadline  = 800 * time.Millisecond
	qualityTestShortMaxRun   = 100 * time.Millisecond
	qualityTestUnreachedMAD  = 1000
)

type qualityTestServerOptions struct {
	http2        bool
	abortLarge   bool
	slurpBounded bool
	smallStatus  int
}

func qualityTestParams() responsivenessParams {
	params := defaultResponsivenessParams()
	params.MAD, params.ID, params.MaxRun = qualityTestMovingWindow, qualityTestInterval, qualityTestMaxRun
	params.IdleProbes, params.InitialProbeRate, params.MaxInflightProbes = qualityTestIdleProbes, qualityTestProbeRate, qualityTestInflightLimit
	params.FinalizeReserve, params.ShutdownGrace = qualityTestReserve, qualityTestGrace
	return params
}

func newQualityTestEngine(t *testing.T, options qualityTestServerOptions) *responsivenessEngine {
	t.Helper()
	mux := http.NewServeMux()
	var server *httptest.Server
	mux.HandleFunc("/config", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(writer, `{"version":1,"test_endpoint":"edge.test","urls":{"large_download_url":"%[1]s/large","small_download_url":"%[1]s/small","upload_url":"%[1]s/slurp"}}`, server.URL)
	})
	mux.HandleFunc("/small", func(writer http.ResponseWriter, _ *http.Request) {
		if options.smallStatus != 0 {
			writer.WriteHeader(options.smallStatus)
			return
		}
		_, _ = writer.Write([]byte{0})
	})
	mux.HandleFunc("/large", func(writer http.ResponseWriter, request *http.Request) {
		chunk := make([]byte, qualityTestChunkBytes)
		flusher, _ := writer.(http.Flusher)
		for written := 0; written < qualityTestFiniteChunks; written++ {
			if request.Context().Err() != nil {
				return
			}
			if _, err := writer.Write(chunk); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			if options.abortLarge && (written+1)*qualityTestChunkBytes >= qualityTestAbortAfter {
				panic(http.ErrAbortHandler)
			}
		}
	})
	mux.HandleFunc("/slurp", func(writer http.ResponseWriter, request *http.Request) {
		body := io.Reader(request.Body)
		if options.slurpBounded {
			body = io.LimitReader(request.Body, qualityTestUploadLimit)
		}
		_, _ = io.Copy(io.Discard, body)
	})
	server = httptest.NewUnstartedServer(mux)
	if options.http2 {
		server.EnableHTTP2 = true
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	base := server.Client().Transport.(*http.Transport)
	engine := newResponsivenessEngine(server.URL + "/config")
	engine.params = qualityTestParams()
	engine.newTransport = func() *http.Transport { return base.Clone() }
	return engine
}

func runQualityTestEngine(t *testing.T, engine *responsivenessEngine, ctx context.Context) Result {
	t.Helper()
	report, outcome, err := engine.Run(ctx)
	result := nativeQualityResult(time.Now(), report, outcome, err)
	if active := engine.active.Load(); active != 0 {
		t.Fatalf("active workers = %d after Run", active)
	}
	return result
}

func TestQualityNativeRun(t *testing.T) {
	engine := newQualityTestEngine(t, qualityTestServerOptions{http2: true, slurpBounded: true})
	ctx, cancel := context.WithTimeout(context.Background(), qualityTestRunTimeout)
	defer cancel()
	result := runQualityTestEngine(t, engine, ctx)
	if result.Status != StatusPass {
		t.Fatalf("result = %+v", result)
	}
	for _, key := range []string{"download_bps", "upload_bps", "responsiveness_rpm", "base_rtt_ms", "source", "confidence", "download_flows", "upload_flows", "config_url", "test_endpoint", "foreign_probes", "self_probes"} {
		if _, ok := result.Metrics[key]; !ok {
			t.Errorf("metrics miss %s: %v", key, result.Metrics)
		}
	}
	if result.Metrics["self_probes"].(int) == 0 || result.Metrics["source"] != qualitySourceNative {
		t.Fatalf("metrics = %v", result.Metrics)
	}
	if !strings.Contains(result.Summary, "↓") || !strings.Contains(result.Summary, "RPM") {
		t.Fatalf("summary = %q", result.Summary)
	}
	if seen := engine.maxInflight.Load(); seen > int64(engine.params.MaxInflightProbes) {
		t.Fatalf("in-flight probes reached %d", seen)
	}
}

func TestQualityNativePartial(t *testing.T) {
	engine := newQualityTestEngine(t, qualityTestServerOptions{http2: true})
	engine.params.MaxRun = qualityTestRunTimeout
	// SDT 0은 안정 판정을 막아 취소 전에 측정이 끝나지 않게 한다.
	engine.params.SDT = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(qualityTestCancelAfter, cancel)
	defer timer.Stop()
	started := time.Now()
	result := runQualityTestEngine(t, engine, ctx)
	if elapsed := time.Since(started); elapsed > qualityTestCancelAfter+engine.params.ShutdownGrace+qualityTestReturnSlack {
		t.Fatalf("Run returned after %s", elapsed)
	}
	if result.Status != StatusWarn || len(result.Warnings) == 0 || result.Warnings[0] != T("observe.quality.warn.partial") {
		t.Fatalf("result = %+v", result)
	}
	if _, ok := result.Metrics["download_bps"]; !ok {
		t.Fatalf("metrics = %v", result.Metrics)
	}
}

func TestQualityNativeCancel(t *testing.T) {
	engine := newQualityTestEngine(t, qualityTestServerOptions{http2: true})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	result := runQualityTestEngine(t, engine, ctx)
	if elapsed := time.Since(started); elapsed > engine.params.ShutdownGrace+qualityTestReturnSlack {
		t.Fatalf("Run returned after %s", elapsed)
	}
	if result.Status != StatusFail || result.Error == nil || result.Error.Kind != "timeout" {
		t.Fatalf("result = %+v", result)
	}
}

func TestQualityNativeHTTP1(t *testing.T) {
	engine := newQualityTestEngine(t, qualityTestServerOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), qualityTestRunTimeout)
	defer cancel()
	result := runQualityTestEngine(t, engine, ctx)
	if result.Status != StatusPass {
		t.Fatalf("result = %+v", result)
	}
	if result.Metrics["self_probes"] != 0 {
		t.Fatalf("self probes = %v", result.Metrics["self_probes"])
	}
	if _, ok := result.Metrics["responsiveness_rpm"]; !ok {
		t.Fatalf("metrics = %v", result.Metrics)
	}
	found := false
	for _, warning := range result.Warnings {
		found = found || warning == T("observe.quality.warn.self_unavailable")
	}
	if !found {
		t.Fatalf("warnings = %q", result.Warnings)
	}
}

func TestQualityNativeLoadError(t *testing.T) {
	engine := newQualityTestEngine(t, qualityTestServerOptions{http2: true, abortLarge: true})
	ctx, cancel := context.WithTimeout(context.Background(), qualityTestRunTimeout)
	defer cancel()
	result := runQualityTestEngine(t, engine, ctx)
	if result.Status != StatusFail || result.Error == nil || result.Error.Kind != "load" {
		t.Fatalf("result = %+v", result)
	}
	if _, ok := result.Metrics["download_bps"]; !ok {
		t.Fatalf("metrics = %v", result.Metrics)
	}
}

func TestQualityNativeConfigFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"version":1}`))
	}))
	defer server.Close()
	engine := newResponsivenessEngine(server.URL)
	engine.params = qualityTestParams()
	result := runQualityTestEngine(t, engine, context.Background())
	if result.Status != StatusFail || result.Error == nil || result.Error.Kind != "config" {
		t.Fatalf("result = %+v", result)
	}
	if engine.downloadBytes.Load() != 0 || engine.uploadBytes.Load() != 0 {
		t.Fatal("load started after a config failure")
	}
}

func TestQualityNativeNoLeak(t *testing.T) {
	engine := newQualityTestEngine(t, qualityTestServerOptions{http2: true})
	ctx, cancel := context.WithTimeout(context.Background(), qualityTestRunTimeout)
	defer cancel()
	runQualityTestEngine(t, engine, ctx)
	engine.workers.Wait()
	if active := engine.active.Load(); active != 0 {
		t.Fatalf("active workers = %d", active)
	}
}

func TestQualityNativeDeadlinePartial(t *testing.T) {
	engine := newQualityTestEngine(t, qualityTestServerOptions{http2: true, slurpBounded: true})
	engine.params.MAD, engine.params.SDT = qualityTestUnreachedMAD, 0
	ctx, cancel := context.WithTimeout(context.Background(), qualityTestShortDeadline)
	defer cancel()
	result := runQualityTestEngine(t, engine, ctx)
	if result.Status != StatusWarn || len(result.Warnings) == 0 || result.Warnings[0] != T("observe.quality.warn.partial") {
		t.Fatalf("result = %+v", result)
	}
	if result.Metrics["confidence"] != string(qualityConfidenceLow) {
		t.Fatalf("metrics = %v", result.Metrics)
	}
}

func TestQualityNativeTimeoutSetsRunLength(t *testing.T) {
	engine := newQualityTestEngine(t, qualityTestServerOptions{http2: true, slurpBounded: true})
	engine.params.MaxRun, engine.params.SDT = qualityTestShortMaxRun, 0
	ctx, cancel := context.WithTimeout(context.Background(), qualityTestLongDeadline)
	defer cancel()
	started := time.Now()
	result := runQualityTestEngine(t, engine, ctx)
	if elapsed := time.Since(started); elapsed < qualityTestLongDeadline-qualityTestReserve-qualityTestInterval {
		t.Fatalf("Run stopped after %s; MaxRun capped a run that had a deadline", elapsed)
	}
	if result.Status != StatusPass {
		t.Fatalf("result = %+v", result)
	}
}

func TestQualityNativeProbeErrorStatus(t *testing.T) {
	engine := newQualityTestEngine(t, qualityTestServerOptions{http2: true, slurpBounded: true, smallStatus: http.StatusServiceUnavailable})
	ctx, cancel := context.WithTimeout(context.Background(), qualityTestShortDeadline)
	defer cancel()
	result := runQualityTestEngine(t, engine, ctx)
	if result.Metrics["foreign_probes"] != 0 || result.Metrics["self_probes"] != 0 {
		t.Fatalf("error responses became samples: %v", result.Metrics)
	}
	for _, key := range []string{"responsiveness_rpm", "base_rtt_ms"} {
		if _, ok := result.Metrics[key]; ok {
			t.Fatalf("metrics have %s from error responses: %v", key, result.Metrics)
		}
	}
	if _, ok := result.Metrics["download_bps"]; !ok {
		t.Fatalf("metrics = %v", result.Metrics)
	}
}

func TestQualityNativeReportURLDropsQuery(t *testing.T) {
	report := nativeQualityReport{ConfigURL: "https://quality.example.net/config?token=secret#frag"}
	result := nativeQualityResult(time.Now(), report, nativeOutcomeConfig, errQualityConfigStatus)
	if got := result.Metrics["config_url"]; got != "https://quality.example.net/config" {
		t.Fatalf("config_url = %v", got)
	}
}

func TestQualityNativeStableExitKeepsWindow(t *testing.T) {
	engine := newQualityTestEngine(t, qualityTestServerOptions{http2: true, slurpBounded: true})
	ctx, cancel := context.WithTimeout(context.Background(), qualityTestRunTimeout)
	defer cancel()
	report, outcome, err := engine.Run(ctx)
	if err != nil || outcome != nativeOutcomePass || report.Confidence != qualityConfidenceHigh {
		t.Skipf("run did not reach stability: outcome=%d confidence=%s err=%v", outcome, report.Confidence, err)
	}
	// interval 0은 부하 시작 때 열린다. 안정 판정이 난 tick은 새 interval을 열지 않아야 한다.
	if got := len(engine.samples); got != report.Intervals {
		t.Fatalf("samples has %d intervals after %d ticks; the stable exit opened an empty one", got, report.Intervals)
	}
}
