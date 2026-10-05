package edc

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	qualityProbeID           = "net.quality"
	qualitySourceNetworkQual = "networkQuality"

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

func probeQuality(ctx context.Context) Result {
	started := time.Now()
	if runtime.GOOS != "darwin" {
		return unsupported(qualityProbeID, T("observe.system.quality_darwin_only"))
	}
	command := exec.CommandContext(ctx, "/usr/bin/networkQuality", "-c")
	output, err := command.Output()
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
