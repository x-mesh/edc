package edc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sort"
	"strings"
	"sync"
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
		Summary:    T("observe.probe.tcp_connected", address, duration.Round(time.Millisecond)) + ", " + T("observe.probe.peer_ip", peerIP(connection.RemoteAddr())),
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
	rootCAs *x509.CertPool
}

type httpCheckOptions struct {
	expectStatus int // 0이면 4xx warn, 5xx fail 기본 규칙을 쓴다
	dialAddress  string
	noRedirect   bool
	rootCAs      *x509.CertPool
	headersOnly  bool
}

func probeTLS(ctx context.Context, address, serverName string) Result {
	return probeTLSWithOptions(ctx, address, serverName, tlsCheckOptions{})
}

func probeTLSWithOptions(ctx context.Context, address, serverName string, options tlsCheckOptions) Result {
	started := time.Now()
	dialer := &tls.Dialer{Config: &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12, RootCAs: options.rootCAs}}
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
		Summary: fmt.Sprintf("%s, %s, %s", tlsVersion(state.Version), tls.CipherSuiteName(state.CipherSuite), T("observe.probe.peer_ip", peerIP(connection.RemoteAddr()))),
		Metrics: metrics,
	}
	if len(state.PeerCertificates) > 0 {
		certificate := state.PeerCertificates[0]
		days := int(time.Until(certificate.NotAfter).Hours() / 24)
		expiry := certificateExpiryLabel(certificate.NotAfter)
		result.Summary += ", " + expiry + " (" + T("observe.probe.days_remaining", days) + ")"
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
			result.Summary = result.Error.Message + ", " + expiry
		}
	}
	result.DurationMS = time.Since(started).Milliseconds()
	return result
}

func peerIP(address net.Addr) string {
	host, _, err := net.SplitHostPort(address.String())
	if err != nil {
		return address.String()
	}
	return host
}

func certificateExpiryLabel(expires time.Time) string {
	return T("observe.probe.certificate_expires_on", expires.UTC().Format("2006-01-02 15:04"))
}

// certificateVerdict는 남은 일수를 --min-days 기준과 기본 경고 기준에 차례로 비교한다.
func certificateVerdict(days, minDays int) (Status, string, *DiagnosticError) {
	if minDays > 0 && days < minDays {
		message := T("observe.probe.certificate_below_minimum", days, minDays)
		return StatusFail, "", &DiagnosticError{Kind: "expiry", Message: message}
	}
	if days < certificateWarnDays {
		return StatusWarn, T("observe.probe.certificate_expiring", days), nil
	}
	return StatusPass, "", nil
}

// httpStatusVerdict는 기대 status가 있으면 일치 여부만 보고, 없으면 4xx warn, 5xx fail 규칙을 쓴다.
func httpStatusVerdict(code, expected int) (Status, *DiagnosticError) {
	if expected > 0 {
		if code != expected {
			return StatusFail, &DiagnosticError{Kind: "status", Message: T("observe.probe.status_mismatch", code, expected)}
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
	rawURL = withHTTPScheme(rawURL)
	var dnsStart, connectStart, tlsStart, wroteRequest time.Time
	timings := map[string]int64{}
	var traceMu sync.Mutex
	var peer string
	var resolvedHost string
	var resolvedAddresses []string
	trace := &httptrace.ClientTrace{
		GetConn: func(string) {
			traceMu.Lock()
			timings = map[string]int64{}
			dnsStart, connectStart, tlsStart, wroteRequest = time.Time{}, time.Time{}, time.Time{}, time.Time{}
			peer = ""
			resolvedHost = ""
			resolvedAddresses = nil
			traceMu.Unlock()
		},
		DNSStart: func(info httptrace.DNSStartInfo) {
			traceMu.Lock()
			dnsStart = time.Now()
			resolvedHost = info.Host
			traceMu.Unlock()
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			traceMu.Lock()
			if !dnsStart.IsZero() {
				timings["dns_ms"] = time.Since(dnsStart).Milliseconds()
			}
			resolvedAddresses = make([]string, 0, len(info.Addrs))
			for _, address := range info.Addrs {
				resolvedAddresses = append(resolvedAddresses, address.IP.String())
			}
			sort.Strings(resolvedAddresses)
			traceMu.Unlock()
		},
		ConnectStart: func(_, _ string) {
			traceMu.Lock()
			connectStart = time.Now()
			traceMu.Unlock()
		},
		ConnectDone: func(_, _ string, _ error) {
			traceMu.Lock()
			if !connectStart.IsZero() {
				timings["connect_ms"] = time.Since(connectStart).Milliseconds()
			}
			traceMu.Unlock()
		},
		TLSHandshakeStart: func() {
			traceMu.Lock()
			tlsStart = time.Now()
			traceMu.Unlock()
		},
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			traceMu.Lock()
			if !tlsStart.IsZero() {
				timings["tls_ms"] = time.Since(tlsStart).Milliseconds()
			}
			traceMu.Unlock()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			traceMu.Lock()
			peer = peerIP(info.Conn.RemoteAddr())
			traceMu.Unlock()
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			traceMu.Lock()
			wroteRequest = time.Now()
			traceMu.Unlock()
		},
		GotFirstResponseByte: func() {
			traceMu.Lock()
			if !wroteRequest.IsZero() {
				timings["ttfb_ms"] = time.Since(wroteRequest).Milliseconds()
			}
			traceMu.Unlock()
		},
	}
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, rawURL, nil)
	if err != nil {
		return resultFromError("http.check", started, "input", err)
	}
	redirects := 0
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	defer transport.CloseIdleConnections()
	if options.rootCAs != nil {
		transport.TLSClientConfig = &tls.Config{RootCAs: options.rootCAs, MinVersion: tls.VersionTLS12}
	}
	if options.dialAddress != "" {
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", options.dialAddress)
		}
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if options.noRedirect {
				return http.ErrUseLastResponse
			}
			if len(via) >= 10 {
				return fmt.Errorf("%s", T("observe.probe.too_many_redirects"))
			}
			redirects = len(via)
			return nil
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return resultFromError("http.check", started, classifyNetworkError(err), err)
	}
	defer response.Body.Close()
	var bytesRead int64
	if !options.headersOnly {
		var readErr error
		bytesRead, readErr = io.Copy(io.Discard, io.LimitReader(response.Body, 10*1024*1024))
		if readErr != nil {
			return resultFromError("http.check", started, "response", readErr)
		}
	}
	total := time.Since(started)
	status, diagnostic := httpStatusVerdict(response.StatusCode, options.expectStatus)
	metrics := map[string]interface{}{"url": rawURL, "final_url": response.Request.URL.String(), "status_code": response.StatusCode, "bytes_read": bytesRead, "total_ms": total.Milliseconds()}
	if options.expectStatus > 0 {
		metrics["expected_status"] = options.expectStatus
	}
	observedTimings := map[string]int64{}
	traceMu.Lock()
	for key, value := range timings {
		metrics[key] = value
		observedTimings[key] = value
	}
	observedPeer := peer
	observedHost := resolvedHost
	observedAddresses := append([]string(nil), resolvedAddresses...)
	if observedPeer != "" {
		metrics["peer_ip"] = observedPeer
	}
	if observedHost != "" {
		metrics["resolved_host"] = observedHost
		metrics["resolved_addresses"] = observedAddresses
	}
	traceMu.Unlock()
	metrics["redirects"] = redirects
	metrics["timing_scope"] = "final_request"
	summary := fmt.Sprintf("HTTP %d, %s, %d bytes", response.StatusCode, total.Round(time.Millisecond), bytesRead)
	if diagnostic != nil {
		summary = fmt.Sprintf("%s, %s, %d bytes", diagnostic.Message, total.Round(time.Millisecond), bytesRead)
	}
	if observedPeer != "" {
		if proxy, _ := http.ProxyFromEnvironment(response.Request); proxy != nil && options.dialAddress == "" {
			metrics["peer_is_proxy"] = true
			summary += ", " + T("observe.probe.proxy_peer_ip", observedPeer)
		} else {
			summary += ", " + T("observe.probe.peer_ip", observedPeer)
		}
	}
	if redirects > 0 {
		summary += ", " + T("observe.probe.final_request")
	}
	for _, phase := range []struct{ key, label string }{{"dns_ms", "DNS"}, {"connect_ms", "TCP"}, {"tls_ms", "TLS"}, {"ttfb_ms", "TTFB"}} {
		if value, ok := observedTimings[phase.key]; ok {
			summary += fmt.Sprintf(", %s %dms", phase.label, value)
		}
	}
	if redirects > 0 {
		summary += ", " + T("observe.probe.http_redirect", redirects, response.Request.URL.String())
	}
	return Result{Probe: "http.check", Status: status, StartedAt: started.UTC(), DurationMS: total.Milliseconds(), Summary: summary, Metrics: metrics, Error: diagnostic}
}

// withHTTPScheme은 스킴이 없는 입력에 http://를 붙인다. http.NewRequest는 스킴 없는 주소를
// `unsupported protocol scheme ""`으로 거부하므로, `edc http check naver.com`이 실패했다.
// 스킴은 url.Parse가 아니라 "://"로 찾는다. "naver.com:8080"을 Parse하면 "naver.com"이 스킴으로 읽힌다.
func withHTTPScheme(input string) string {
	if strings.Contains(input, "://") {
		return input
	}
	if net.ParseIP(input) != nil && strings.Contains(input, ":") {
		return "http://[" + input + "]"
	}
	return "http://" + input
}

func normalizeTarget(input string) (host, address, rawURL string, err error) {
	if strings.Contains(input, "://") {
		parsed, parseErr := url.Parse(input)
		if parseErr != nil || parsed.Hostname() == "" {
			return "", "", "", fmt.Errorf("%s", T("observe.probe.invalid_url", input))
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return "", "", "", fmt.Errorf("%s", T("observe.probe.unsupported_scheme", input))
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
