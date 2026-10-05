package edc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	qualityProbeID        = "net.quality"
	defaultQualityTimeout = 30 * time.Second
	defaultQualityServer  = "https://mensura.cdn-apple.com/api/v1/gm/config"
	networkQualityPath    = "/usr/bin/networkQuality"
	// networkQuality가 -M을 모르면 이 문구를 stderr에 쓰고 측정 없이 곧바로 끝난다.
	networkQualityNoMaxRuntime = "invalid option -- M"
	qualitySourceNetworkQual   = "networkQuality"

	bitsPerKilobit = 1000
	bitsPerMegabit = 1000 * bitsPerKilobit
	bitsPerGigabit = 1000 * bitsPerMegabit

	qualityWholeUnitThreshold = 10
	thousandsGroupSize        = 3

	qualityPartSeparator  = "  "
	qualityGroupSeparator = "  ·  "
)

type qualityMeasurement struct {
	DownloadBPS       *float64
	UploadBPS         *float64
	ResponsivenessRPM *float64
	BaseRTTMS         *float64
	Interface         string
	Source            string
}

func (m qualityMeasurement) hasHeadline() bool {
	return m.DownloadBPS != nil || m.UploadBPS != nil || m.ResponsivenessRPM != nil
}

func mergeQualityMetrics(metrics map[string]interface{}, m qualityMeasurement) {
	optional := map[string]*float64{
		"download_bps":       m.DownloadBPS,
		"upload_bps":         m.UploadBPS,
		"responsiveness_rpm": m.ResponsivenessRPM,
		"base_rtt_ms":        m.BaseRTTMS,
	}
	for key, value := range optional {
		if value != nil {
			metrics[key] = *value
		}
	}
	if m.Interface != "" {
		metrics["interface"] = m.Interface
	}
	if m.Source != "" {
		metrics["source"] = m.Source
	}
}

func normalizeNetworkQuality(raw map[string]interface{}) qualityMeasurement {
	m := qualityMeasurement{
		DownloadBPS:       qualityNumber(raw["dl_throughput"]),
		UploadBPS:         qualityNumber(raw["ul_throughput"]),
		ResponsivenessRPM: qualityNumber(raw["responsiveness"]),
		BaseRTTMS:         qualityNumber(raw["base_rtt"]),
		Source:            qualitySourceNetworkQual,
	}
	if name, ok := raw["interface_name"].(string); ok {
		m.Interface = strings.TrimSpace(name)
	}
	return m
}

// qualityNumber는 측정값으로 쓸 수 없는 값을 버린다. 0은 실제로 잰 값일 수 있어 남긴다.
func qualityNumber(value interface{}) *float64 {
	var number float64
	switch typed := value.(type) {
	case float64:
		number = typed
	case float32:
		number = float64(typed)
	case int:
		number = float64(typed)
	case int64:
		number = float64(typed)
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return nil
		}
		number = parsed
	default:
		return nil
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
		return nil
	}
	return &number
}

func qualitySummary(m qualityMeasurement) string {
	var rates []string
	if m.DownloadBPS != nil {
		rates = append(rates, "↓ "+formatBitRate(*m.DownloadBPS))
	}
	if m.UploadBPS != nil {
		rates = append(rates, "↑ "+formatBitRate(*m.UploadBPS))
	}
	var groups []string
	if len(rates) > 0 {
		groups = append(groups, strings.Join(rates, qualityPartSeparator))
	}
	if m.ResponsivenessRPM != nil {
		groups = append(groups, groupThousands(int64(math.Round(*m.ResponsivenessRPM)))+" RPM")
	}
	return strings.Join(groups, qualityGroupSeparator)
}

func formatBitRate(bps float64) string {
	value, unit := bps/bitsPerKilobit, "Kbps"
	switch {
	case bps >= bitsPerGigabit:
		value, unit = bps/bitsPerGigabit, "Gbps"
	case bps >= bitsPerMegabit:
		value, unit = bps/bitsPerMegabit, "Mbps"
	}
	if value >= qualityWholeUnitThreshold {
		return fmt.Sprintf("%.0f %s", value, unit)
	}
	return fmt.Sprintf("%.1f %s", value, unit)
}

func groupThousands(value int64) string {
	digits := strconv.FormatInt(value, 10)
	var builder strings.Builder
	for index, digit := range digits {
		if index > 0 && (len(digits)-index)%thousandsGroupSize == 0 {
			builder.WriteByte(',')
		}
		builder.WriteRune(digit)
	}
	return builder.String()
}

// resolveQualityTimeout은 defaults.common.timeout을 물려받지 않는다. 예전 setup이 쓴 common 15s가 측정을 매번 자르기 때문이다.
func resolveQualityTimeout() time.Duration {
	return configuredDuration(activeConfig.Defaults.Quality.Timeout, defaultQualityTimeout)
}

// doctorTimeout은 full profile이 quality를 함께 돌릴 때만 기한을 늘린다. 명시한 --timeout은 그대로 둔다.
func doctorTimeout(profile string, timeoutFlagSet bool, current time.Duration) time.Duration {
	if timeoutFlagSet || profile != "full" {
		return current
	}
	if quality := resolveQualityTimeout(); quality > current {
		return quality
	}
	return current
}

func runQuality(args []string, version string) int {
	return runQualityWith(args, version, qualityProbe)
}

func runQualityWith(args []string, version string, newProbe func(server string) func(context.Context) Result) int {
	options := configuredCommon(defaultQualityTimeout)
	options.timeout = resolveQualityTimeout()
	server := configuredString(activeConfig.Defaults.Quality.Server, "")
	flags := probeFlags{
		bind: func(set *flag.FlagSet) {
			set.StringVar(&server, "server", server, T("command.quality.option.server"))
		},
		check: func() error {
			if server == "" {
				return nil
			}
			if err := validateQualityServer(server); err != nil {
				return errors.New(T("cli.error.quality_server", server))
			}
			return nil
		},
	}
	return runSimpleWithOptions(args, version, "quality", qualityProbeID, options, flags, func(ctx context.Context) Result {
		return newProbe(server)(ctx)
	})
}

// qualityProbe는 빈 server를 기본 서버로 본다. macOS에서는 사용자가 바꾼 경우에만 -C를 넘겨 networkQuality의 기본 동작을 지킨다.
func qualityProbe(server string) func(context.Context) Result {
	if runtime.GOOS == "darwin" {
		return func(ctx context.Context) Result { return probeNetworkQuality(ctx, server, runNetworkQuality) }
	}
	configURL := server
	if configURL == "" {
		configURL = defaultQualityServer
	}
	return func(ctx context.Context) Result { return probeNativeQuality(ctx, configURL) }
}

func networkQualityArgs(server string, maxRuntime time.Duration) []string {
	args := []string{"-c"}
	if maxRuntime >= time.Second {
		args = append(args, "-M", strconv.Itoa(int(maxRuntime/time.Second)))
	}
	if server != "" {
		args = append(args, "-C", server)
	}
	return args
}

// networkQualityMaxRuntime은 기한 전에 networkQuality가 스스로 끝내게 한다. 기한에 kill되면 측정값이 하나도 남지 않는다.
func networkQualityMaxRuntime(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	return time.Until(deadline) - finalizeReserve
}

func runNetworkQuality(ctx context.Context, args []string) ([]byte, error) {
	return exec.CommandContext(ctx, networkQualityPath, args...).Output()
}

func probeNetworkQuality(ctx context.Context, server string, run func(context.Context, []string) ([]byte, error)) Result {
	started := time.Now()
	output, err := run(ctx, networkQualityArgs(server, networkQualityMaxRuntime(ctx)))
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && bytes.Contains(exitErr.Stderr, []byte(networkQualityNoMaxRuntime)) {
		output, err = run(ctx, networkQualityArgs(server, 0))
	}
	if err != nil {
		return resultFromError(qualityProbeID, started, classifyCommandError(ctx, err), err)
	}
	return networkQualityResult(started, output)
}

func networkQualityResult(started time.Time, output []byte) Result {
	var metrics map[string]interface{}
	if err := json.Unmarshal(output, &metrics); err != nil {
		return resultFromError(qualityProbeID, started, "parse", err)
	}
	if metrics == nil {
		metrics = map[string]interface{}{}
	}
	measurement := normalizeNetworkQuality(metrics)
	mergeQualityMetrics(metrics, measurement)
	return qualityResult(started, measurement, metrics)
}

func qualityResult(started time.Time, m qualityMeasurement, metrics map[string]interface{}) Result {
	result := Result{Probe: qualityProbeID, Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(), Summary: qualitySummary(m), Metrics: metrics}
	if !m.hasHeadline() {
		result.Status = StatusWarn
		result.Summary = T("observe.system.quality_done")
		result.Warnings = append(result.Warnings, T("observe.quality.warn.no_metrics"))
	}
	return result
}
