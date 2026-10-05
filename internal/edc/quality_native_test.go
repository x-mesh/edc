package edc

import (
	"context"
	"crypto/tls"
	"errors"
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
