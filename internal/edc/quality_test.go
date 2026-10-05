package edc

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// networkQualityFixture는 macOS 27의 `networkQuality -c` 실제 출력이다. 배열은 앞 세 개만 남겼다.
const networkQualityFixture = `{
  "base_rtt" : 11.239375114440918,
  "cli_options" : [ "-c" ],
  "dl_bytes_transferred" : 121299311,
  "dl_flows" : 8,
  "dl_phase_duration" : 15.136159062385559,
  "dl_phase_end" : "2026-10-05 18:29:01.643",
  "dl_phase_start" : "2026-10-05 18:28:46.506",
  "dl_throughput" : 67181720,
  "draft_version" : 8,
  "end_date" : "2026-10-05 18:29:01.647",
  "il_h2_req_resp" : [ 13.326048851013184, 11.948943138122559, 17.189979553222656 ],
  "il_tcp_handshake_443" : [ 4, 4, 5 ],
  "il_tls_handshake" : [ 12, 12, 13 ],
  "interface_name" : "en1",
  "lud_foreign_h2_req_resp" : [ 145, 140, 138 ],
  "lud_foreign_tcp_handshake_443" : [ 57, 96, 16 ],
  "lud_foreign_tls_handshake" : [ 19, 25, 26 ],
  "lud_self_h2_req_resp" : [ 109, 67, 26 ],
  "os_version" : "Version 27.0.1 (Build 26A434)",
  "other" : { "protocols_seen" : { "h2" : 308 } },
  "responsiveness" : 189.62039184570312,
  "start_date" : "2026-10-05 18:28:46.411",
  "ul_bytes_transferred" : 214040574,
  "ul_flows" : 6,
  "ul_phase_duration" : 15.136159062385559,
  "ul_phase_end" : "2026-10-05 18:29:01.643",
  "ul_phase_start" : "2026-10-05 18:28:46.506",
  "ul_throughput" : 110664168
}`

func qualityFloat(value float64) *float64 { return &value }

func TestQualityNormalizeNetworkQuality(t *testing.T) {
	cases := []struct {
		name         string
		output       string
		status       Status
		summary      string
		present      []string
		absent       []string
		warningCount int
	}{
		{
			name:    "real output",
			output:  networkQualityFixture,
			status:  StatusPass,
			summary: "↓ 67 Mbps  ↑ 111 Mbps  ·  190 RPM",
			present: []string{"download_bps", "upload_bps", "responsiveness_rpm", "base_rtt_ms", "interface", "source", "dl_throughput", "lud_self_h2_req_resp"},
		},
		{
			name:    "missing upload and rtt",
			output:  `{"dl_throughput": 4200000, "responsiveness": 1180.13}`,
			status:  StatusPass,
			summary: "↓ 4.2 Mbps  ·  1,180 RPM",
			present: []string{"download_bps", "responsiveness_rpm", "source"},
			absent:  []string{"upload_bps", "base_rtt_ms", "interface"},
		},
		{
			name:         "string values",
			output:       `{"dl_throughput": "67181720", "ul_throughput": "fast", "responsiveness": "190", "interface_name": "en0"}`,
			status:       StatusWarn,
			summary:      T("observe.system.quality_done"),
			present:      []string{"dl_throughput", "interface", "source"},
			absent:       []string{"download_bps", "upload_bps", "responsiveness_rpm"},
			warningCount: 1,
		},
		{
			name:         "negative values",
			output:       `{"dl_throughput": -1, "ul_throughput": -5, "responsiveness": -10, "base_rtt": -2}`,
			status:       StatusWarn,
			summary:      T("observe.system.quality_done"),
			absent:       []string{"download_bps", "upload_bps", "responsiveness_rpm", "base_rtt_ms"},
			warningCount: 1,
		},
		{
			name:         "empty object",
			output:       `{}`,
			status:       StatusWarn,
			summary:      T("observe.system.quality_done"),
			present:      []string{"source"},
			absent:       []string{"download_bps", "upload_bps", "responsiveness_rpm", "base_rtt_ms", "interface"},
			warningCount: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := networkQualityResult(time.Now(), []byte(tc.output))
			if result.Status != tc.status {
				t.Fatalf("status = %s, want %s", result.Status, tc.status)
			}
			if result.Summary != tc.summary {
				t.Fatalf("summary = %q, want %q", result.Summary, tc.summary)
			}
			if len(result.Warnings) != tc.warningCount {
				t.Fatalf("warnings = %q, want %d", result.Warnings, tc.warningCount)
			}
			if tc.warningCount > 0 && result.Warnings[0] != T("observe.quality.warn.no_metrics") {
				t.Fatalf("warning = %q", result.Warnings[0])
			}
			for _, key := range tc.present {
				if _, ok := result.Metrics[key]; !ok {
					t.Errorf("metrics miss %s: %v", key, result.Metrics)
				}
			}
			for _, key := range tc.absent {
				if value, ok := result.Metrics[key]; ok {
					t.Errorf("metrics carry %s = %v", key, value)
				}
			}
		})
	}
}

func TestQualityNormalizeValues(t *testing.T) {
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(networkQualityFixture), &raw); err != nil {
		t.Fatal(err)
	}
	m := normalizeNetworkQuality(raw)
	if m.DownloadBPS == nil || *m.DownloadBPS != 67181720 {
		t.Fatalf("download = %v", m.DownloadBPS)
	}
	if m.UploadBPS == nil || *m.UploadBPS != 110664168 {
		t.Fatalf("upload = %v", m.UploadBPS)
	}
	if m.BaseRTTMS == nil || *m.BaseRTTMS != 11.239375114440918 {
		t.Fatalf("base rtt = %v", m.BaseRTTMS)
	}
	if m.Interface != "en1" || m.Source != qualitySourceNetworkQual {
		t.Fatalf("interface = %q, source = %q", m.Interface, m.Source)
	}
}

func TestQualityNormalizeNumericKinds(t *testing.T) {
	raw := map[string]interface{}{
		"dl_throughput":  json.Number("1000000"),
		"ul_throughput":  json.Number("not-a-number"),
		"responsiveness": 0.0,
		"base_rtt":       nil,
	}
	m := normalizeNetworkQuality(raw)
	if m.DownloadBPS == nil || *m.DownloadBPS != 1000000 {
		t.Fatalf("download = %v", m.DownloadBPS)
	}
	if m.UploadBPS != nil || m.BaseRTTMS != nil {
		t.Fatalf("upload = %v, base rtt = %v", m.UploadBPS, m.BaseRTTMS)
	}
	if m.ResponsivenessRPM == nil || *m.ResponsivenessRPM != 0 {
		t.Fatalf("responsiveness = %v", m.ResponsivenessRPM)
	}
}

func TestQualityNormalizeParseError(t *testing.T) {
	result := networkQualityResult(time.Now(), []byte("not json"))
	if result.Status != StatusFail || result.Error == nil || result.Error.Kind != "parse" {
		t.Fatalf("result = %+v", result)
	}
}

func TestQualitySummaryParts(t *testing.T) {
	cases := []struct {
		name string
		m    qualityMeasurement
		want string
	}{
		{"full", qualityMeasurement{DownloadBPS: qualityFloat(450e6), UploadBPS: qualityFloat(42e6), ResponsivenessRPM: qualityFloat(1180.13)}, "↓ 450 Mbps  ↑ 42 Mbps  ·  1,180 RPM"},
		{"gigabit", qualityMeasurement{DownloadBPS: qualityFloat(1.26e9), UploadBPS: qualityFloat(12.4e9)}, "↓ 1.3 Gbps  ↑ 12 Gbps"},
		{"kilobit", qualityMeasurement{UploadBPS: qualityFloat(600), ResponsivenessRPM: qualityFloat(12345678)}, "↑ 0.6 Kbps  ·  12,345,678 RPM"},
		{"rpm only", qualityMeasurement{ResponsivenessRPM: qualityFloat(999.6)}, "1,000 RPM"},
		{"empty", qualityMeasurement{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := qualitySummary(tc.m); got != tc.want {
				t.Fatalf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestQualitySummaryResultLine(t *testing.T) {
	result := networkQualityResult(time.Now(), []byte(networkQualityFixture))
	line := formatResultLine(result, false)
	if !strings.Contains(line, "↓") || !strings.Contains(line, "RPM") {
		t.Fatalf("result line = %q", line)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Metrics map[string]interface{} `json:"metrics"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Metrics["dl_throughput"] != float64(67181720) {
		t.Fatalf("dl_throughput = %v", decoded.Metrics["dl_throughput"])
	}
	if decoded.Metrics["download_bps"] != float64(67181720) || decoded.Metrics["source"] != qualitySourceNetworkQual {
		t.Fatalf("metrics = %v", decoded.Metrics)
	}
}

func TestQualityTimeoutPrecedence(t *testing.T) {
	restore := activeConfig
	defer func() { activeConfig = restore }()
	const commonTimeout, qualityTimeout = 12 * time.Second, 45 * time.Second
	cases := []struct {
		name     string
		defaults configDefaults
		args     []string
		want     time.Duration
	}{
		{"builtin", configDefaults{}, nil, defaultQualityTimeout},
		{"common ignored", configDefaults{Common: commonConfig{Timeout: durationPointer(commonTimeout)}}, nil, defaultQualityTimeout},
		{"quality over common", configDefaults{Common: commonConfig{Timeout: durationPointer(commonTimeout)}, Quality: qualityConfig{Timeout: durationPointer(qualityTimeout)}}, nil, qualityTimeout},
		{"flag over both", configDefaults{Common: commonConfig{Timeout: durationPointer(commonTimeout)}, Quality: qualityConfig{Timeout: durationPointer(qualityTimeout)}}, []string{"--timeout", "7s"}, 7 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			activeConfig = edcConfig{Defaults: tc.defaults}
			var remaining time.Duration
			probe := func(ctx context.Context) Result {
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Fatal("probe context has no deadline")
				}
				remaining = time.Until(deadline)
				return Result{Probe: qualityProbeID, Status: StatusPass}
			}
			args := append([]string{"--json", filepath.Join(t.TempDir(), "report.json")}, tc.args...)
			if code := runQualityWith(args, "test", func(string) func(context.Context) Result { return probe }); code != 0 {
				t.Fatalf("exit = %d", code)
			}
			if remaining > tc.want || remaining < tc.want-time.Second {
				t.Fatalf("timeout = %s, want %s", remaining, tc.want)
			}
		})
	}
}

func TestQualityDoctorTimeout(t *testing.T) {
	restore := activeConfig
	defer func() { activeConfig = restore }()
	const shortTimeout, longTimeout = 15 * time.Second, 90 * time.Second
	cases := []struct {
		name    string
		quality *configDuration
		profile string
		flagSet bool
		current time.Duration
		want    time.Duration
	}{
		{"default profile keeps current", nil, "default", false, shortTimeout, shortTimeout},
		{"full raises to builtin quality", nil, "full", false, shortTimeout, defaultQualityTimeout},
		{"full raises to configured quality", durationPointer(longTimeout), "full", false, shortTimeout, longTimeout},
		{"full never lowers", nil, "full", false, longTimeout, longTimeout},
		{"explicit flag wins", durationPointer(longTimeout), "full", true, shortTimeout, shortTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			activeConfig = edcConfig{Defaults: configDefaults{Quality: qualityConfig{Timeout: tc.quality}}}
			if got := doctorTimeout(tc.profile, tc.flagSet, tc.current); got != tc.want {
				t.Fatalf("timeout = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestQualityConfigRejectsNonPositiveTimeout(t *testing.T) {
	for _, value := range []time.Duration{0, -time.Second} {
		config := edcConfig{Defaults: configDefaults{Quality: qualityConfig{Timeout: durationPointer(value)}}}
		if err := validateConfig(config); err == nil || !strings.Contains(err.Error(), "defaults.quality.timeout") {
			t.Fatalf("timeout %s: error = %v", value, err)
		}
	}
	if recommended := recommendedConfig().Defaults.Quality.Timeout; recommended == nil || recommended.Duration != defaultQualityTimeout {
		t.Fatalf("recommended quality timeout = %v", recommended)
	}
}

func TestQualityServerPrecedence(t *testing.T) {
	restore := activeConfig
	defer func() { activeConfig = restore }()
	const configured, explicit = "https://configured.example/config", "http://explicit.example:8080/config"
	cases := []struct {
		name   string
		config *string
		args   []string
		want   string
		code   int
	}{
		{"builtin", nil, nil, "", 0},
		{"config", stringPointer(configured), nil, configured, 0},
		{"flag over config", stringPointer(configured), []string{"--server", explicit}, explicit, 0},
		{"relative flag", nil, []string{"--server", "relative/config"}, "", 2},
		{"file flag", nil, []string{"--server", "file:///etc/config"}, "", 2},
		{"empty host flag", nil, []string{"--server", "https://"}, "", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			activeConfig = edcConfig{Defaults: configDefaults{Quality: qualityConfig{Server: tc.config}}}
			got, measured := "", false
			newProbe := func(server string) func(context.Context) Result {
				return func(context.Context) Result {
					got, measured = server, true
					return Result{Probe: qualityProbeID, Status: StatusPass}
				}
			}
			args := append([]string{"--json", filepath.Join(t.TempDir(), "report.json")}, tc.args...)
			if code := runQualityWith(args, "test", newProbe); code != tc.code {
				t.Fatalf("exit = %d, want %d", code, tc.code)
			}
			if tc.code != 0 {
				if measured {
					t.Fatal("an invalid server still ran the measurement")
				}
				return
			}
			if got != tc.want {
				t.Fatalf("server = %q, want %q", got, tc.want)
			}
		})
	}
	for _, server := range []string{"relative/config", "ftp://h.example/config"} {
		config := edcConfig{Defaults: configDefaults{Quality: qualityConfig{Server: stringPointer(server)}}}
		if err := validateConfig(config); err == nil || !strings.Contains(err.Error(), "defaults.quality.server") {
			t.Fatalf("server %q: error = %v", server, err)
		}
	}
	if err := validateConfig(edcConfig{Defaults: configDefaults{Quality: qualityConfig{Server: stringPointer("")}}}); err != nil {
		t.Fatalf("empty server: %v", err)
	}
}

func TestQualityDarwinArgs(t *testing.T) {
	const server = "https://quality.example.net/config"
	cases := []struct {
		name       string
		server     string
		maxRuntime time.Duration
		want       string
	}{
		{"default", "", 0, "-c"},
		{"override", server, 0, "-c -C " + server},
		{"max runtime", "", 27900 * time.Millisecond, "-c -M 27"},
		{"max runtime and override", server, 27 * time.Second, "-c -M 27 -C " + server},
		{"under a second", "", 900 * time.Millisecond, "-c"},
	}
	for _, tc := range cases {
		if got := strings.Join(networkQualityArgs(tc.server, tc.maxRuntime), " "); got != tc.want {
			t.Fatalf("%s: args = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestQualityDarwinMaxRuntimeFromDeadline(t *testing.T) {
	const timeout = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var calls [][]string
	run := func(_ context.Context, args []string) ([]byte, error) {
		calls = append(calls, args)
		return []byte(`{"dl_throughput": 1000000}`), nil
	}
	if result := probeNetworkQuality(ctx, "", run); result.Status != StatusPass {
		t.Fatalf("result = %+v", result)
	}
	if len(calls) != 1 || strings.Join(calls[0][:2], " ") != "-c -M" {
		t.Fatalf("calls = %q", calls)
	}
	seconds, err := strconv.Atoi(calls[0][2])
	if err != nil || time.Duration(seconds)*time.Second > timeout-finalizeReserve {
		t.Fatalf("-M %q does not leave the finalize reserve before the deadline", calls[0][2])
	}
}

func TestQualityDarwinMaxRuntimeFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultQualityTimeout)
	defer cancel()
	var calls [][]string
	run := func(_ context.Context, args []string) ([]byte, error) {
		calls = append(calls, args)
		if slices.Contains(args, "-M") {
			return nil, &exec.ExitError{Stderr: []byte("networkQuality: " + networkQualityNoMaxRuntime + "\nUSAGE: networkQuality ...\n")}
		}
		return []byte(`{"dl_throughput": 1000000}`), nil
	}
	result := probeNetworkQuality(ctx, "", run)
	if result.Status != StatusPass || len(calls) != 2 || slices.Contains(calls[1], "-M") {
		t.Fatalf("result = %+v, calls = %q", result, calls)
	}
}

func TestQualityDarwinOtherFailureNoRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultQualityTimeout)
	defer cancel()
	calls := 0
	run := func(context.Context, []string) ([]byte, error) {
		calls++
		return nil, &exec.ExitError{Stderr: []byte("networkQuality: connection failed\n")}
	}
	if result := probeNetworkQuality(ctx, "", run); result.Status != StatusFail || calls != 1 {
		t.Fatalf("result = %+v, calls = %d", result, calls)
	}
}
