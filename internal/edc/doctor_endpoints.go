package edc

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

const endpointProbeID = "endpoints.check"
const endpointConcurrency = 4

type endpointCheck struct {
	IP         string          `json:"ip"`
	TCP        Status          `json:"tcp"`
	ConnectMS  int64           `json:"connect_ms,omitempty"`
	TLS        Status          `json:"tls"`
	TLSVersion string          `json:"tls_version,omitempty"`
	HTTP       Status          `json:"http"`
	HTTPCode   int             `json:"http_code,omitempty"`
	Detail     string          `json:"detail,omitempty"`
	Network    endpointNetwork `json:"network"`
}

type endpointOptions struct {
	rootCAs     *x509.CertPool
	ownerLookup func(context.Context, string) endpointNetwork
}

func probeAllIPs(ctx context.Context, dns Result, host, port, rawURL string) Result {
	return probeAllIPsWithOptions(ctx, dns, host, port, rawURL, endpointOptions{})
}

func probeAllIPsWithOptions(ctx context.Context, dns Result, host, port, rawURL string, options endpointOptions) Result {
	started := time.Now()
	if options.ownerLookup == nil {
		options.ownerLookup = newEndpointOwnerResolver(ripeStatURL).lookup
	}
	if dns.Status != StatusPass {
		return unsupported(endpointProbeID, T("observe.probe.endpoints_dns_failed"))
	}
	addresses, ok := dns.Metrics["addresses"].([]string)
	if !ok || len(addresses) == 0 {
		return unsupported(endpointProbeID, T("observe.probe.endpoints_empty"))
	}
	checks := make([]endpointCheck, len(addresses))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(len(addresses), endpointConcurrency) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				checks[index] = checkEndpoint(ctx, addresses[index], host, port, rawURL, options)
			}
		}()
	}
	for index := range addresses {
		select {
		case jobs <- index:
		case <-ctx.Done():
			for skipped := index; skipped < len(addresses); skipped++ {
				checks[skipped] = endpointCheck{IP: addresses[skipped], TCP: StatusSkip, TLS: StatusSkip, HTTP: StatusSkip, Detail: ctx.Err().Error()}
			}
			close(jobs)
			workers.Wait()
			return endpointResult(started, checks)
		}
	}
	close(jobs)
	workers.Wait()
	return endpointResult(started, checks)
}

func checkEndpoint(ctx context.Context, ip, host, port, rawURL string, options endpointOptions) (check endpointCheck) {
	address := net.JoinHostPort(ip, port)
	check = endpointCheck{IP: ip, TLS: StatusSkip, HTTP: StatusSkip}
	defer func() { check.Network = options.ownerLookup(ctx, ip) }()
	tcp := probeTCP(ctx, address)
	check.TCP = tcp.Status
	if value, ok := tcp.Metrics["connect_ms"].(int64); ok {
		check.ConnectMS = value
	}
	if tcp.Status == StatusFail {
		check.Detail = firstLine(tcp.Summary)
		return check
	}
	if strings.HasPrefix(rawURL, "https://") {
		tls := probeTLSWithOptions(ctx, address, host, tlsCheckOptions{rootCAs: options.rootCAs})
		check.TLS = tls.Status
		if value, ok := tls.Metrics["version"].(string); ok {
			check.TLSVersion = value
		}
		if tls.Status == StatusFail {
			check.Detail = firstLine(tls.Summary)
			return check
		}
	}
	http := probeHTTPWithOptions(ctx, rawURL, httpCheckOptions{dialAddress: address, noRedirect: true, rootCAs: options.rootCAs, headersOnly: true})
	check.HTTP = http.Status
	if value, ok := http.Metrics["status_code"].(int); ok {
		check.HTTPCode = value
	}
	if http.Status == StatusFail {
		check.Detail = firstLine(http.Summary)
	}
	return check
}

func endpointResult(started time.Time, checks []endpointCheck) Result {
	var passed, warned, failed, skipped int
	for _, check := range checks {
		switch endpointStatus(check) {
		case StatusPass:
			passed++
		case StatusWarn:
			warned++
		case StatusFail:
			failed++
		case StatusSkip:
			skipped++
		}
	}
	status := StatusPass
	if failed > 0 {
		status = StatusFail
	} else if warned > 0 || skipped > 0 {
		status = StatusWarn
	}
	summary := T("observe.probe.endpoints_summary", len(checks), passed, warned, failed, skipped)
	result := Result{
		Probe: endpointProbeID, Status: status, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(),
		Summary:  summary,
		Metrics:  map[string]interface{}{"endpoints": checks, "pass": passed, "warn": warned, "fail": failed, "skip": skipped, "owner_source": "RIPEstat", "load_balancer": "unknown"},
		Evidence: []Evidence{{Label: T("observe.probe.endpoints_details"), Value: endpointTable(checks)}},
	}
	if failed > 0 {
		result.Error = &DiagnosticError{Kind: "endpoint", Message: summary}
	}
	return result
}

func endpointStatus(check endpointCheck) Status {
	if check.TCP == StatusFail || check.TLS == StatusFail || check.HTTP == StatusFail {
		return StatusFail
	}
	if check.TCP == StatusSkip {
		return StatusSkip
	}
	if check.TCP == StatusWarn || check.TLS == StatusWarn || check.HTTP == StatusWarn {
		return StatusWarn
	}
	return StatusPass
}

func endpointTable(checks []endpointCheck) string {
	var table strings.Builder
	table.WriteString("IP                               TCP       TLS       HTTP  ASN       OWNER / DETAIL\n")
	for _, check := range checks {
		tcp := strings.ToUpper(string(check.TCP))
		if check.TCP == StatusPass || check.TCP == StatusWarn {
			tcp = fmt.Sprintf("%dms", check.ConnectMS)
		}
		tls := strings.ToUpper(string(check.TLS))
		if check.TLSVersion != "" {
			tls = check.TLSVersion
		}
		http := strings.ToUpper(string(check.HTTP))
		if check.HTTPCode != 0 {
			http = fmt.Sprint(check.HTTPCode)
		}
		asns := make([]string, 0, len(check.Network.ASNs))
		for _, asn := range check.Network.ASNs {
			asns = append(asns, "AS"+asn)
		}
		owner := endpointOwnerLabel(check.Network)
		if check.Detail != "" {
			owner += " · " + check.Detail
		}
		fmt.Fprintf(&table, "%-32s %-9s %-9s %-5s %-9s %s\n", check.IP, tcp, tls, http, strings.Join(asns, ","), owner)
	}
	table.WriteString(T("observe.probe.owner_source") + "\n")
	table.WriteString(T("observe.probe.lb_unknown"))
	return strings.TrimRight(table.String(), "\n")
}
