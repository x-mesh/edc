package edc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"
)

const (
	qualitySourceNative = "native"
	probesPerPair       = 2
)

var (
	errQualityLoadStatus      = errors.New("load request failed")
	errQualityProbeIncomplete = errors.New("probe finished without connection timings")
	errQualityProbeStatus     = errors.New("probe request failed")
)

type nativeQualityOutcome int

const (
	nativeOutcomePass nativeQualityOutcome = iota
	nativeOutcomePartial
	nativeOutcomeTimeout
	nativeOutcomeLoad
	nativeOutcomeConfig
)

type loadDirection int

const (
	loadDownload loadDirection = iota
	loadUpload
)

type nativeQualityReport struct {
	Measurement     qualityMeasurement
	Confidence      qualityConfidence
	DownloadFlows   int
	UploadFlows     int
	ConfigURL       string
	TestEndpoint    string
	ForeignProbes   int
	SelfProbes      int
	SkippedProbes   int
	SelfUnavailable bool
}

type responsivenessEngine struct {
	configURL    string
	params       responsivenessParams
	newTransport func() *http.Transport
	now          func() time.Time

	active        atomic.Int64
	inflight      atomic.Int64
	maxInflight   atomic.Int64
	downloadBytes atomic.Int64
	uploadBytes   atomic.Int64
	workers       sync.WaitGroup
	loadErrOnce   sync.Once

	mu              sync.Mutex
	interval        int
	samples         []responsivenessSamples
	transports      []*http.Transport
	flows           map[loadDirection]int
	foreignProbes   int
	selfProbes      int
	skippedProbes   int
	selfUnavailable bool
	probeGoodput    float64
	localIP         net.IP
	loadErr         error
}

type loadProgress struct {
	download  []goodputInterval
	upload    []goodputInterval
	completed int
	stable    bool
}

type foreignSample struct {
	tcp    float64
	tls    float64
	http   float64
	hasTLS bool
	local  net.Addr
}

func newResponsivenessEngine(configURL string) *responsivenessEngine {
	return &responsivenessEngine{configURL: configURL, params: defaultResponsivenessParams(), newTransport: defaultQualityTransport, now: time.Now}
}

func defaultQualityTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = true
	return transport
}

func probeNativeQuality(ctx context.Context, configURL string) Result {
	started := time.Now()
	report, outcome, err := newResponsivenessEngine(configURL).Run(ctx)
	return nativeQualityResult(started, report, outcome, err)
}

func (e *responsivenessEngine) Run(ctx context.Context) (nativeQualityReport, nativeQualityOutcome, error) {
	report := nativeQualityReport{ConfigURL: e.configURL}
	e.flows = map[loadDirection]int{}
	runEnd := time.Now().Add(e.params.MaxRun)
	if deadline, ok := ctx.Deadline(); ok {
		runEnd = deadline.Add(-e.params.FinalizeReserve)
	}
	runCtx, cancelRun := context.WithDeadline(ctx, runEnd)
	defer cancelRun()

	configTransport := e.newTransport()
	config, err := fetchResponsivenessConfig(runCtx, &http.Client{Transport: configTransport}, e.configURL)
	configTransport.CloseIdleConnections()
	if err != nil {
		if runCtx.Err() != nil {
			return report, nativeOutcomeTimeout, err
		}
		return report, nativeOutcomeConfig, err
	}
	report.TestEndpoint = config.TestEndpoint

	baseRTT := e.idlePhase(runCtx, config.SmallDownloadURL)
	progress := e.loadPhase(runCtx, cancelRun, config)
	e.shutdown(cancelRun)

	measurement := qualityMeasurement{BaseRTTMS: baseRTT, Source: qualitySourceNative}
	if e.downloadBytes.Load() > 0 {
		value := movingGoodput(progress.download, e.params.MAD)
		measurement.DownloadBPS = &value
	}
	if e.uploadBytes.Load() > 0 {
		value := movingGoodput(progress.upload, e.params.MAD)
		measurement.UploadBPS = &value
	}
	e.mu.Lock()
	if rpm, ok := responsivenessRPM(recentSamples(e.samples, e.params.MAD), e.params.TMP); ok {
		measurement.ResponsivenessRPM = &rpm
	}
	report.DownloadFlows, report.UploadFlows = e.flows[loadDownload], e.flows[loadUpload]
	report.ForeignProbes, report.SelfProbes, report.SkippedProbes = e.foreignProbes, e.selfProbes, e.skippedProbes
	report.SelfUnavailable = e.selfUnavailable
	localIP, loadErr := e.localIP, e.loadErr
	e.mu.Unlock()
	measurement.Interface = interfaceForIP(localIP)
	report.Measurement = measurement
	report.Confidence = measurementConfidence(progress.completed, e.params.MAD, progress.stable)

	switch {
	case loadErr != nil:
		return report, nativeOutcomeLoad, loadErr
	case !measurement.hasHeadline():
		if ctx.Err() != nil {
			return report, nativeOutcomeTimeout, ctx.Err()
		}
		return report, nativeOutcomeTimeout, errors.New(T("observe.quality.error.no_data"))
	case ctx.Err() != nil, progress.completed < e.params.MAD:
		return report, nativeOutcomePartial, nil
	default:
		return report, nativeOutcomePass, nil
	}
}

func (e *responsivenessEngine) idlePhase(ctx context.Context, url string) *float64 {
	var httpTimes []float64
	for index := 0; index < e.params.IdleProbes && ctx.Err() == nil; index++ {
		sample, err := e.foreignProbe(ctx, url)
		if err != nil {
			continue
		}
		e.noteLocal(sample.local)
		httpTimes = append(httpTimes, sample.http)
	}
	if mean, ok := trimmedMean(httpTimes, e.params.TMP); ok {
		return &mean
	}
	return nil
}

func (e *responsivenessEngine) loadPhase(ctx context.Context, abort context.CancelFunc, config responsivenessConfig) loadProgress {
	e.mu.Lock()
	e.samples = []responsivenessSamples{{}}
	e.mu.Unlock()
	for index := 0; index < e.params.INP; index++ {
		e.addLoad(ctx, abort, config, loadDownload)
		e.addLoad(ctx, abort, config, loadUpload)
	}
	e.startWorker(func() { e.launchProbes(ctx, config.SmallDownloadURL) })

	var progress loadProgress
	var downloadAverages, uploadAverages, rpmAverages []float64
	lastTick := e.now()
	var lastDownload, lastUpload int64
	snapshot := func() {
		now := e.now()
		download, upload := e.downloadBytes.Load(), e.uploadBytes.Load()
		elapsed := now.Sub(lastTick)
		progress.download = append(progress.download, goodputInterval{Bytes: download - lastDownload, Duration: elapsed})
		progress.upload = append(progress.upload, goodputInterval{Bytes: upload - lastUpload, Duration: elapsed})
		lastTick, lastDownload, lastUpload = now, download, upload
	}
	ticker := time.NewTicker(e.params.ID)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			snapshot()
			return progress
		case <-ticker.C:
		}
		snapshot()
		progress.completed++
		downloadAverages = append(downloadAverages, movingGoodput(progress.download, e.params.MAD))
		uploadAverages = append(uploadAverages, movingGoodput(progress.upload, e.params.MAD))
		e.mu.Lock()
		window := recentSamples(e.samples, e.params.MAD)
		e.samples = append(e.samples, responsivenessSamples{})
		e.interval++
		e.probeGoodput = downloadAverages[len(downloadAverages)-1] + uploadAverages[len(uploadAverages)-1]
		e.mu.Unlock()

		downloadStable := stable(downloadAverages, e.params.MAD, e.params.SDT)
		uploadStable := stable(uploadAverages, e.params.MAD, e.params.SDT)
		for index := 0; index < e.params.INC; index++ {
			if !downloadStable {
				e.addLoad(ctx, abort, config, loadDownload)
			}
			if !uploadStable {
				e.addLoad(ctx, abort, config, loadUpload)
			}
		}
		if rpm, ok := responsivenessRPM(window, e.params.TMP); ok {
			rpmAverages = append(rpmAverages, rpm)
		}
		if downloadStable && uploadStable && stable(rpmAverages, e.params.MAD, e.params.SDT) {
			progress.stable = true
			return progress
		}
	}
}

func (e *responsivenessEngine) shutdown(cancel context.CancelFunc) {
	cancel()
	done := make(chan struct{})
	go func() {
		e.workers.Wait()
		close(done)
	}()
	grace := time.NewTimer(e.params.ShutdownGrace)
	defer grace.Stop()
	select {
	case <-done:
	case <-grace.C:
	}
	e.mu.Lock()
	transports := append([]*http.Transport(nil), e.transports...)
	e.mu.Unlock()
	for _, transport := range transports {
		transport.CloseIdleConnections()
	}
}

func (e *responsivenessEngine) startWorker(work func()) {
	e.workers.Add(1)
	e.active.Add(1)
	go func() {
		defer e.workers.Done()
		defer e.active.Add(-1)
		work()
	}()
}

func (e *responsivenessEngine) addLoad(ctx context.Context, abort context.CancelFunc, config responsivenessConfig, direction loadDirection) {
	if ctx.Err() != nil {
		return
	}
	transport := e.newTransport()
	transport.DisableCompression = true
	e.mu.Lock()
	if e.flows[direction] >= e.params.MNP {
		e.mu.Unlock()
		return
	}
	e.flows[direction]++
	e.transports = append(e.transports, transport)
	e.mu.Unlock()
	client := &http.Client{Transport: transport}
	e.startWorker(func() {
		for ctx.Err() == nil {
			var err error
			if direction == loadDownload {
				err = e.downloadOnce(ctx, client, config.LargeDownloadURL)
			} else {
				err = e.uploadOnce(ctx, client, config.UploadURL)
			}
			if err != nil {
				e.failLoad(ctx, abort, err)
				return
			}
		}
	})
}

// failLoad는 draft대로 연결 오류에서 측정을 멈춘다. 취소가 만든 오류는 오류가 아니다.
func (e *responsivenessEngine) failLoad(ctx context.Context, abort context.CancelFunc, err error) {
	if ctx.Err() != nil {
		return
	}
	e.loadErrOnce.Do(func() {
		e.mu.Lock()
		e.loadErr = err
		e.mu.Unlock()
		abort()
	})
}

func (e *responsivenessEngine) downloadOnce(ctx context.Context, client *http.Client, url string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("%w: %s", errQualityLoadStatus, response.Status)
	}
	_, err = io.Copy(byteCounter{total: &e.downloadBytes}, response.Body)
	return err
}

func (e *responsivenessEngine) uploadOnce(ctx context.Context, client *http.Client, url string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, zeroReader{ctx: ctx, total: &e.uploadBytes})
	if err != nil {
		return err
	}
	request.ContentLength = -1
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		return err
	}
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("%w: %s", errQualityLoadStatus, response.Status)
	}
	return nil
}

func (e *responsivenessEngine) launchProbes(ctx context.Context, url string) {
	for {
		e.mu.Lock()
		rate := probePairsPerSecond(e.params, e.probeGoodput)
		e.mu.Unlock()
		wait := e.params.ID
		if rate > 0 {
			// 아주 느린 링크에서도 interval마다 표본이 생기도록 간격은 ID를 넘지 않는다.
			wait = min(time.Duration(float64(time.Second)/rate), e.params.ID)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		e.mu.Lock()
		selfAvailable := !e.selfUnavailable && len(e.transports) > 0
		var selfTransport *http.Transport
		if selfAvailable {
			selfTransport = e.transports[rand.IntN(len(e.transports))]
		}
		needed := int64(1)
		if selfAvailable {
			needed = probesPerPair
		}
		if e.inflight.Load()+needed > int64(e.params.MaxInflightProbes) {
			e.skippedProbes++
			e.mu.Unlock()
			continue
		}
		e.mu.Unlock()
		e.startProbe(func() { e.runForeignProbe(ctx, url) })
		if selfAvailable {
			e.startProbe(func() { e.runSelfProbe(ctx, selfTransport, url) })
		}
	}
}

func (e *responsivenessEngine) startProbe(probe func()) {
	current := e.inflight.Add(1)
	for {
		seen := e.maxInflight.Load()
		if current <= seen || e.maxInflight.CompareAndSwap(seen, current) {
			break
		}
	}
	e.startWorker(func() {
		defer e.inflight.Add(-1)
		probe()
	})
}

// 표본은 probe가 끝난 interval에 넣는다. 띄운 interval에 넣으면 종료 직전에 띄운 probe가 shutdown에 취소되어 마지막 window가 빌 수 있다.
func (e *responsivenessEngine) runForeignProbe(ctx context.Context, url string) {
	sample, err := e.foreignProbe(ctx, url)
	if err != nil {
		return
	}
	e.noteLocal(sample.local)
	e.mu.Lock()
	defer e.mu.Unlock()
	current := &e.samples[e.interval]
	current.TCPForeign = append(current.TCPForeign, sample.tcp)
	if sample.hasTLS {
		current.TLSForeign = append(current.TLSForeign, sample.tls)
	}
	current.HTTPForeign = append(current.HTTPForeign, sample.http)
	e.foreignProbes++
}

// runSelfProbe는 부하 연결 위에 요청을 섞는다. HTTP/2가 아니면 새 연결이 열리므로 http_l로 쓰지 않는다.
func (e *responsivenessEngine) runSelfProbe(ctx context.Context, transport *http.Transport, url string) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	started := e.now()
	response, err := transport.RoundTrip(request)
	if err != nil {
		return
	}
	_, err = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	elapsed := e.now().Sub(started)
	e.mu.Lock()
	defer e.mu.Unlock()
	if response.ProtoMajor != 2 {
		e.selfUnavailable = true
		return
	}
	if err != nil || response.StatusCode/100 != 2 {
		return
	}
	current := &e.samples[e.interval]
	current.HTTPLoaded = append(current.HTTPLoaded, milliseconds(elapsed))
	e.selfProbes++
}

func (e *responsivenessEngine) foreignProbe(ctx context.Context, url string) (foreignSample, error) {
	transport := e.newTransport()
	transport.DisableKeepAlives = true
	defer transport.CloseIdleConnections()
	// dial goroutine이 RoundTrip이 끝난 뒤에도 trace를 부를 수 있어 시각을 모두 잠근다.
	var mu sync.Mutex
	var connectStart, connectDone, tlsStart, tlsDone, gotConn time.Time
	var version uint16
	var local net.Addr
	trace := &httptrace.ClientTrace{
		ConnectStart: func(string, string) {
			mu.Lock()
			defer mu.Unlock()
			if connectStart.IsZero() {
				connectStart = e.now()
			}
		},
		ConnectDone: func(_, _ string, err error) {
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			connectDone = e.now()
		},
		TLSHandshakeStart: func() {
			mu.Lock()
			defer mu.Unlock()
			tlsStart = e.now()
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			tlsDone, version = e.now(), state.Version
		},
		GotConn: func(info httptrace.GotConnInfo) {
			mu.Lock()
			defer mu.Unlock()
			gotConn, local = e.now(), info.Conn.LocalAddr()
		},
	}
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, url, nil)
	if err != nil {
		return foreignSample{}, err
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		return foreignSample{}, err
	}
	_, err = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	finished := e.now()
	if err != nil {
		return foreignSample{}, err
	}
	if response.StatusCode/100 != 2 {
		return foreignSample{}, fmt.Errorf("%w: %s", errQualityProbeStatus, response.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	if connectStart.IsZero() || connectDone.IsZero() || gotConn.IsZero() {
		return foreignSample{}, errQualityProbeIncomplete
	}
	sample := foreignSample{tcp: milliseconds(connectDone.Sub(connectStart)), http: milliseconds(finished.Sub(gotConn)), local: local}
	if !tlsStart.IsZero() && !tlsDone.IsZero() {
		sample.tls, sample.hasTLS = normalizeTLSHandshake(tlsDone.Sub(tlsStart), version), true
	}
	return sample, nil
}

func (e *responsivenessEngine) noteLocal(address net.Addr) {
	tcp, ok := address.(*net.TCPAddr)
	if !ok {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.localIP == nil {
		e.localIP = tcp.IP
	}
}

func interfaceForIP(ip net.IP) string {
	if ip == nil {
		return ""
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range interfaces {
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			if network, ok := address.(*net.IPNet); ok && network.IP.Equal(ip) {
				return iface.Name
			}
		}
	}
	return ""
}

func recentSamples(intervals []responsivenessSamples, mad int) responsivenessSamples {
	if mad > 0 && len(intervals) > mad {
		intervals = intervals[len(intervals)-mad:]
	}
	var merged responsivenessSamples
	for _, interval := range intervals {
		merged.TCPForeign = append(merged.TCPForeign, interval.TCPForeign...)
		merged.TLSForeign = append(merged.TLSForeign, interval.TLSForeign...)
		merged.HTTPForeign = append(merged.HTTPForeign, interval.HTTPForeign...)
		merged.HTTPLoaded = append(merged.HTTPLoaded, interval.HTTPLoaded...)
	}
	return merged
}

type byteCounter struct{ total *atomic.Int64 }

func (counter byteCounter) Write(data []byte) (int, error) {
	counter.total.Add(int64(len(data)))
	return len(data), nil
}

// zeroReader는 끝없는 upload 본문이다. 취소되면 오류를 돌려 transport가 본문 쓰기를 멈추게 한다.
type zeroReader struct {
	ctx   context.Context
	total *atomic.Int64
}

func (reader zeroReader) Read(data []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	clear(data)
	reader.total.Add(int64(len(data)))
	return len(data), nil
}

func nativeQualityResult(started time.Time, report nativeQualityReport, outcome nativeQualityOutcome, err error) Result {
	metrics := map[string]interface{}{"config_url": qualityReportURL(report.ConfigURL)}
	if report.TestEndpoint != "" {
		metrics["test_endpoint"] = report.TestEndpoint
	}
	var warnings []string
	if outcome != nativeOutcomeConfig {
		mergeQualityMetrics(metrics, report.Measurement)
		metrics["confidence"] = string(report.Confidence)
		metrics["download_flows"] = report.DownloadFlows
		metrics["upload_flows"] = report.UploadFlows
		metrics["foreign_probes"] = report.ForeignProbes
		metrics["self_probes"] = report.SelfProbes
		metrics["skipped_probes"] = report.SkippedProbes
		if report.SelfUnavailable {
			warnings = append(warnings, T("observe.quality.warn.self_unavailable"))
		}
	}
	kind := ""
	switch outcome {
	case nativeOutcomeConfig:
		kind = "config"
	case nativeOutcomeTimeout:
		kind = "timeout"
	case nativeOutcomeLoad:
		kind = "load"
	}
	if kind != "" {
		result := resultFromError(qualityProbeID, started, kind, err)
		result.Metrics, result.Warnings = metrics, warnings
		return result
	}
	result := Result{Probe: qualityProbeID, Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(),
		Summary: qualitySummary(report.Measurement) + qualityGroupSeparator + qualityConfidenceLabel(report.Confidence), Metrics: metrics, Warnings: warnings}
	if outcome == nativeOutcomePartial {
		result.Status = StatusWarn
		result.Warnings = append([]string{T("observe.quality.warn.partial")}, result.Warnings...)
	}
	return result
}

func qualityConfidenceLabel(confidence qualityConfidence) string {
	switch confidence {
	case qualityConfidenceHigh:
		return T("observe.quality.confidence.high")
	case qualityConfidenceMedium:
		return T("observe.quality.confidence.medium")
	default:
		return T("observe.quality.confidence.low")
	}
}
