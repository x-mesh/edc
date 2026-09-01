package edc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sort"
	"strings"
	"time"
)

func probeDNS(ctx context.Context, name string) Result {
	started := time.Now()
	addresses, err := net.DefaultResolver.LookupHost(ctx, name)
	if err != nil {
		return resultFromError("dns.lookup", started, "dns", err)
	}
	sort.Strings(addresses)
	cname, _ := net.DefaultResolver.LookupCNAME(ctx, name)
	return Result{
		Probe: "dns.lookup", Status: StatusPass, StartedAt: started.UTC(),
		DurationMS: time.Since(started).Milliseconds(),
		Summary:    fmt.Sprintf("%s → %s", name, strings.Join(addresses, ", ")),
		Metrics:    map[string]interface{}{"name": name, "addresses": addresses, "cname": cname},
	}
}

func probeTCP(ctx context.Context, address string) Result {
	started := time.Now()
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return resultFromError("tcp.check", started, classifyNetworkError(err), err)
	}
	defer connection.Close()
	duration := time.Since(started)
	return Result{
		Probe: "tcp.check", Status: latencyStatus(duration), StartedAt: started.UTC(),
		DurationMS: duration.Milliseconds(),
		Summary:    fmt.Sprintf("%s 연결 성공 (%s)", address, duration.Round(time.Millisecond)),
		Metrics: map[string]interface{}{
			"address": address, "connect_ms": duration.Milliseconds(),
			"local_address": connection.LocalAddr().String(), "remote_address": connection.RemoteAddr().String(),
		},
	}
}

// certificateWarnDays는 --min-days와 무관하게 항상 적용되는 만료 경고 기준이다.
const certificateWarnDays = 30

type tlsCheckOptions struct {
	minDays int // 인증서 남은 일수가 이 값보다 작으면 fail, 0은 비활성
}

type httpCheckOptions struct {
	expectStatus int // 0이면 4xx warn, 5xx fail 기본 규칙을 쓴다
}

func probeTLS(ctx context.Context, address, serverName string) Result {
	return probeTLSWithOptions(ctx, address, serverName, tlsCheckOptions{})
}

func probeTLSWithOptions(ctx context.Context, address, serverName string, options tlsCheckOptions) Result {
	started := time.Now()
	dialer := &tls.Dialer{Config: &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return resultFromError("tls.check", started, "tls", err)
	}
	defer connection.Close()
	state := connection.(*tls.Conn).ConnectionState()
	metrics := map[string]interface{}{
		"address": address, "server_name": serverName,
		"version": tlsVersion(state.Version), "cipher_suite": tls.CipherSuiteName(state.CipherSuite),
	}
	result := Result{
		Probe: "tls.check", Status: StatusPass, StartedAt: started.UTC(),
		Summary: fmt.Sprintf("%s, %s", tlsVersion(state.Version), tls.CipherSuiteName(state.CipherSuite)),
		Metrics: metrics,
	}
	if len(state.PeerCertificates) > 0 {
		certificate := state.PeerCertificates[0]
		days := int(time.Until(certificate.NotAfter).Hours() / 24)
		metrics["certificate_subject"] = certificate.Subject.CommonName
		metrics["certificate_expires_at"] = certificate.NotAfter.UTC()
		metrics["certificate_days_remaining"] = days
		if options.minDays > 0 {
			metrics["min_days"] = options.minDays
		}
		var warning string
		result.Status, warning, result.Error = certificateVerdict(days, options.minDays)
		if warning != "" {
			result.Warnings = append(result.Warnings, warning)
		}
		if result.Error != nil {
			result.Summary = result.Error.Message
		}
	}
	result.DurationMS = time.Since(started).Milliseconds()
	return result
}

// certificateVerdict는 남은 일수를 --min-days 기준과 기본 경고 기준에 차례로 비교한다.
func certificateVerdict(days, minDays int) (Status, string, *DiagnosticError) {
	if minDays > 0 && days < minDays {
		message := fmt.Sprintf("인증서 만료까지 %d일 남아 기준 %d일보다 짧습니다", days, minDays)
		return StatusFail, "", &DiagnosticError{Kind: "expiry", Message: message}
	}
	if days < certificateWarnDays {
		return StatusWarn, fmt.Sprintf("인증서 만료까지 %d일 남았습니다", days), nil
	}
	return StatusPass, "", nil
}

// httpStatusVerdict는 기대 status가 있으면 일치 여부만 보고, 없으면 4xx warn, 5xx fail 규칙을 쓴다.
func httpStatusVerdict(code, expected int) (Status, *DiagnosticError) {
	if expected > 0 {
		if code != expected {
			return StatusFail, &DiagnosticError{Kind: "status", Message: fmt.Sprintf("HTTP %d, 기대값 %d", code, expected)}
		}
		return StatusPass, nil
	}
	switch {
	case code >= 500:
		return StatusFail, nil
	case code >= 400:
		return StatusWarn, nil
	}
	return StatusPass, nil
}

func probeHTTP(ctx context.Context, rawURL string) Result {
	return probeHTTPWithOptions(ctx, rawURL, httpCheckOptions{})
}

func probeHTTPWithOptions(ctx context.Context, rawURL string, options httpCheckOptions) Result {
	started := time.Now()
	var dnsStart, connectStart, tlsStart, wroteRequest time.Time
	timings := map[string]int64{}
	trace := &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:              func(httptrace.DNSDoneInfo) { timings["dns_ms"] = time.Since(dnsStart).Milliseconds() },
		ConnectStart:         func(_, _ string) { connectStart = time.Now() },
		ConnectDone:          func(_, _ string, _ error) { timings["connect_ms"] = time.Since(connectStart).Milliseconds() },
		TLSHandshakeStart:    func() { tlsStart = time.Now() },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { timings["tls_ms"] = time.Since(tlsStart).Milliseconds() },
		WroteRequest:         func(httptrace.WroteRequestInfo) { wroteRequest = time.Now() },
		GotFirstResponseByte: func() { timings["ttfb_ms"] = time.Since(wroteRequest).Milliseconds() },
	}
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, rawURL, nil)
	if err != nil {
		return resultFromError("http.check", started, "input", err)
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("redirect가 10회를 초과했습니다")
			}
			return nil
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return resultFromError("http.check", started, classifyNetworkError(err), err)
	}
	defer response.Body.Close()
	bytesRead, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 10*1024*1024))
	if readErr != nil {
		return resultFromError("http.check", started, "response", readErr)
	}
	total := time.Since(started)
	status, diagnostic := httpStatusVerdict(response.StatusCode, options.expectStatus)
	metrics := map[string]interface{}{"url": rawURL, "final_url": response.Request.URL.String(), "status_code": response.StatusCode, "bytes_read": bytesRead, "total_ms": total.Milliseconds()}
	if options.expectStatus > 0 {
		metrics["expected_status"] = options.expectStatus
	}
	for key, value := range timings {
		metrics[key] = value
	}
	summary := fmt.Sprintf("HTTP %d, %s, %d bytes", response.StatusCode, total.Round(time.Millisecond), bytesRead)
	if diagnostic != nil {
		summary = fmt.Sprintf("%s, %s, %d bytes", diagnostic.Message, total.Round(time.Millisecond), bytesRead)
	}
	return Result{Probe: "http.check", Status: status, StartedAt: started.UTC(), DurationMS: total.Milliseconds(), Summary: summary, Metrics: metrics, Error: diagnostic}
}

func normalizeTarget(input string) (host, address, rawURL string, err error) {
	if strings.Contains(input, "://") {
		parsed, parseErr := url.Parse(input)
		if parseErr != nil || parsed.Hostname() == "" {
			return "", "", "", fmt.Errorf("유효하지 않은 URL: %s", input)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return "", "", "", fmt.Errorf("http 또는 https URL만 지원합니다: %s", input)
		}
		host = parsed.Hostname()
		port := parsed.Port()
		if port == "" {
			if parsed.Scheme == "http" {
				port = "80"
			} else {
				port = "443"
			}
		}
		return host, net.JoinHostPort(host, port), parsed.String(), nil
	}
	if parsedHost, parsedPort, splitErr := net.SplitHostPort(input); splitErr == nil {
		host = parsedHost
		address = net.JoinHostPort(parsedHost, parsedPort)
		return host, address, "https://" + address, nil
	}
	host = input
	urlHost := host
	if net.ParseIP(host) != nil && strings.Contains(host, ":") {
		urlHost = "[" + host + "]"
	}
	return host, net.JoinHostPort(host, "443"), "https://" + urlHost, nil
}

func classifyNetworkError(err error) string {
	if err == context.DeadlineExceeded || strings.Contains(strings.ToLower(err.Error()), "timeout") {
		return "timeout"
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return "dns"
	}
	if strings.Contains(strings.ToLower(err.Error()), "refused") {
		return "connection_refused"
	}
	return "network"
}

func latencyStatus(duration time.Duration) Status {
	if duration > time.Second {
		return StatusWarn
	}
	return StatusPass
}

func tlsVersion(version uint16) string {
	switch version {
	case tls.VersionTLS13:
		return "TLS 1.3"
	case tls.VersionTLS12:
		return "TLS 1.2"
	default:
		return fmt.Sprintf("TLS 0x%x", version)
	}
}
