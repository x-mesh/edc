package edc

import (
	"crypto/tls"
	"math"
	"sort"
	"time"
)

// draft-ietf-ippm-responsiveness-09 Table 1의 기본값이다.
const (
	defaultMovingAverageDistance = 4
	defaultIntervalDuration      = 5 * time.Second
	defaultTrimmedMeanPercentile = 0.95
	defaultStdDevTolerance       = 0.05
	defaultInitialConnections    = 1
	defaultConnectionIncrement   = 1
	defaultMaxConnections        = 16
	defaultMaxProbesPerSecond    = 100
	defaultProbeCapacityShare    = 0.05

	foreignProbeBytes = 5000
	selfProbeBytes    = 1000
)

const (
	// networkQuality가 부하 전 idle 표본을 8개 남긴다. 같은 수로 base RTT를 잰다.
	idleProbeCount = 8
	// goodput을 모르는 첫 interval에도 RPM 표본이 쌓이도록 낮은 속도로 시작한다.
	initialProbePairsPerSecond = 10
	// 지연이 큰 경로에서 응답이 늦어도 probe goroutine이 무한히 쌓이지 않게 막는다.
	maxInflightProbes = 32
	// 기한 직전에 측정을 멈춰 표본 계산과 결과 출력을 기한 안에 끝낸다.
	finalizeReserve = 2 * time.Second
	shutdownGrace   = time.Second
	// 기본 30s timeout 안에서 idle probe와 마무리 시간을 남긴다.
	maxRun = 20 * time.Second
	// config는 URL 몇 개뿐이다. 이보다 크면 잘못되었거나 악의적인 응답이다.
	configBodyLimit = 64 << 10
)

const (
	millisecondsPerMinute = 60000
	bitsPerByte           = 8
	tls13RoundTrips       = 1
	tls12RoundTrips       = 2
)

type qualityConfidence string

const (
	qualityConfidenceLow    qualityConfidence = "low"
	qualityConfidenceMedium qualityConfidence = "medium"
	qualityConfidenceHigh   qualityConfidence = "high"
)

type responsivenessParams struct {
	MAD int
	ID  time.Duration
	TMP float64
	SDT float64
	INP int
	INC int
	MNP int
	MPS int
	PTC float64

	IdleProbes        int
	InitialProbeRate  float64
	MaxInflightProbes int
	FinalizeReserve   time.Duration
	ShutdownGrace     time.Duration
	MaxRun            time.Duration
}

func defaultResponsivenessParams() responsivenessParams {
	return responsivenessParams{
		MAD: defaultMovingAverageDistance, ID: defaultIntervalDuration, TMP: defaultTrimmedMeanPercentile, SDT: defaultStdDevTolerance,
		INP: defaultInitialConnections, INC: defaultConnectionIncrement, MNP: defaultMaxConnections, MPS: defaultMaxProbesPerSecond, PTC: defaultProbeCapacityShare,
		IdleProbes: idleProbeCount, InitialProbeRate: initialProbePairsPerSecond, MaxInflightProbes: maxInflightProbes,
		FinalizeReserve: finalizeReserve, ShutdownGrace: shutdownGrace, MaxRun: maxRun,
	}
}

// responsivenessSamples는 draft의 probe 측정값이다. 단위는 모두 ms다.
type responsivenessSamples struct {
	TCPForeign  []float64
	TLSForeign  []float64
	HTTPForeign []float64
	HTTPLoaded  []float64
}

// trimmedMean은 draft의 단측 trimmed mean이다. 느린 쪽 (1-TMP) 비율만 버린다.
func trimmedMean(samples []float64, tmp float64) (float64, bool) {
	if len(samples) == 0 {
		return 0, false
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	drop := int(math.Floor(float64(len(sorted)) * (1 - tmp)))
	kept := sorted[:len(sorted)-drop]
	if len(kept) == 0 {
		kept = sorted[:1]
	}
	sum := 0.0
	for _, value := range kept {
		sum += value
	}
	return sum / float64(len(kept)), true
}

func responsivenessRPM(samples responsivenessSamples, tmp float64) (float64, bool) {
	foreign, foreignOK := foreignResponsiveness(samples, tmp)
	loaded, loadedOK := perMinute(trimmedMean(samples.HTTPLoaded, tmp))
	switch {
	case foreignOK && loadedOK:
		return (foreign + loaded) / 2, true
	case foreignOK:
		return foreign, true
	case loadedOK:
		return loaded, true
	default:
		return 0, false
	}
}

func foreignResponsiveness(samples responsivenessSamples, tmp float64) (float64, bool) {
	tcp, tcpOK := trimmedMean(samples.TCPForeign, tmp)
	httpTime, httpOK := trimmedMean(samples.HTTPForeign, tmp)
	if !tcpOK || !httpOK {
		return 0, false
	}
	if tlsTime, tlsOK := trimmedMean(samples.TLSForeign, tmp); tlsOK {
		return perMinute((tcp+tlsTime+httpTime)/3, true)
	}
	return perMinute((tcp+httpTime)/2, true)
}

func perMinute(milliseconds float64, ok bool) (float64, bool) {
	if !ok || milliseconds <= 0 {
		return 0, false
	}
	return millisecondsPerMinute / milliseconds, true
}

// normalizeTLSHandshake는 TLS 버전마다 다른 왕복 수로 handshake 시간을 나눈다.
func normalizeTLSHandshake(duration time.Duration, version uint16) float64 {
	roundTrips := tls12RoundTrips
	if version >= tls.VersionTLS13 {
		roundTrips = tls13RoundTrips
	}
	return float64(duration) / float64(time.Millisecond) / float64(roundTrips)
}

type goodputInterval struct {
	Bytes    int64
	Duration time.Duration
}

// movingGoodput은 최근 MAD개 interval의 전송량을 그 시간으로 나눈 bit/s다.
func movingGoodput(intervals []goodputInterval, mad int) float64 {
	if mad > 0 && len(intervals) > mad {
		intervals = intervals[len(intervals)-mad:]
	}
	var bytes int64
	var covered time.Duration
	for _, interval := range intervals {
		bytes += interval.Bytes
		covered += interval.Duration
	}
	if covered <= 0 {
		return 0
	}
	return float64(bytes) * bitsPerByte / covered.Seconds()
}

// stable은 최근 MAD개 이동 평균의 모표준편차가 현재 값의 SDT 비율보다 작은지 본다.
func stable(movingAverages []float64, mad int, sdt float64) bool {
	if mad <= 0 || len(movingAverages) < mad {
		return false
	}
	window := movingAverages[len(movingAverages)-mad:]
	current := window[len(window)-1]
	if current <= 0 {
		return false
	}
	mean := 0.0
	for _, value := range window {
		mean += value
	}
	mean /= float64(len(window))
	variance := 0.0
	for _, value := range window {
		variance += (value - mean) * (value - mean)
	}
	variance /= float64(len(window))
	return math.Sqrt(variance) < sdt*current
}

func measurementConfidence(intervals, mad int, isStable bool) qualityConfidence {
	switch {
	case intervals < mad:
		return qualityConfidenceLow
	case isStable:
		return qualityConfidenceHigh
	default:
		return qualityConfidenceMedium
	}
}

// probePairsPerSecond는 foreign/self probe 쌍의 발사 속도다. MPS와 goodput의 PTC 비율 중 작은 쪽을 쓴다.
func probePairsPerSecond(params responsivenessParams, goodputBitsPerSecond float64) float64 {
	if goodputBitsPerSecond <= 0 {
		return math.Min(params.InitialProbeRate, float64(params.MPS))
	}
	byCapacity := params.PTC * goodputBitsPerSecond / bitsPerByte / (foreignProbeBytes + selfProbeBytes)
	return math.Min(float64(params.MPS), byCapacity)
}
