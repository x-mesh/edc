package edc

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

type resolverFlags []string

func (flags *resolverFlags) String() string { return strings.Join(*flags, ",") }

func (flags *resolverFlags) Set(value string) error {
	address := value
	if host, _, err := net.SplitHostPort(value); err == nil {
		address = host
	}
	if net.ParseIP(address) == nil {
		return fmt.Errorf("%s", T("observe.dns_compare.resolver_ip", value))
	}
	*flags = append(*flags, value)
	return nil
}

type dnsCompareRow struct {
	Resolver   string   `json:"resolver"`
	IPv4       []string `json:"ipv4"`
	IPv6       []string `json:"ipv6"`
	CNAME      string   `json:"cname,omitempty"`
	TTLSeconds *uint32  `json:"ttl_seconds,omitempty"`
	Status     string   `json:"status"`
	Error      string   `json:"error,omitempty"`
}

func runDNSCompare(args []string, version string) int {
	options := configuredCommon(15 * time.Second)
	set := flag.NewFlagSet("dns compare", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	var resolvers resolverFlags
	set.Var(&resolvers, "resolver", T("command.dns.option.resolver"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 1 {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc dns compare [--resolver IP[:port]] [options] <host>"))
		return 2
	}
	if len(resolvers) == 0 {
		resolvers = resolverFlags{"1.1.1.1"}
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()
	result := probeDNSCompare(ctx, set.Arg(0), resolvers)
	return emit(options, buildReport(version, started, map[string]interface{}{"host": set.Arg(0), "resolvers": []string(resolvers)}, []Result{result}, options.redact))
}

func probeDNSCompare(ctx context.Context, host string, resolvers []string) Result {
	started := time.Now()
	rows := make([]dnsCompareRow, len(resolvers)+1)
	var workers sync.WaitGroup
	workers.Add(len(rows))
	go func() {
		defer workers.Done()
		rows[0] = systemDNSRow(ctx, host)
	}()
	for index, resolver := range resolvers {
		go func() {
			defer workers.Done()
			rows[index+1] = directDNSRow(ctx, host, resolver)
		}()
	}
	workers.Wait()
	matched := true
	for _, row := range rows[1:] {
		if dnsRowSignature(row) != dnsRowSignature(rows[0]) {
			matched = false
		}
	}
	status := StatusPass
	if rows[0].Status != "ok" {
		status = StatusFail
	} else if !matched {
		status = StatusWarn
	}
	summary := T("observe.dns_compare.summary", len(rows), matched)
	result := Result{Probe: "dns.compare", Status: status, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(), Summary: summary,
		Metrics: map[string]interface{}{"rows": rows, "matched": matched}, Evidence: []Evidence{{Label: T("observe.dns_compare.details"), Value: dnsCompareTable(rows)}}}
	if status == StatusFail {
		result.Error = &DiagnosticError{Kind: "dns", Message: rows[0].Error}
		if result.Error.Message == "" {
			result.Error.Message = rows[0].Status
		}
	}
	return result
}

func systemDNSRow(ctx context.Context, host string) dnsCompareRow {
	row := dnsCompareRow{Resolver: "system", IPv4: []string{}, IPv6: []string{}, Status: "ok"}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		row.Status, row.Error = dnsLookupError(err)
		return row
	}
	for _, address := range addresses {
		if address.IP.To4() != nil {
			row.IPv4 = append(row.IPv4, address.IP.String())
		} else {
			row.IPv6 = append(row.IPv6, address.IP.String())
		}
	}
	row.CNAME, _ = net.DefaultResolver.LookupCNAME(ctx, host)
	row.CNAME = normalizeCNAME(row.CNAME, host)
	sort.Strings(row.IPv4)
	sort.Strings(row.IPv6)
	return row
}

func directDNSRow(ctx context.Context, host, resolver string) dnsCompareRow {
	row := dnsCompareRow{Resolver: resolver, IPv4: []string{}, IPv6: []string{}, Status: "ok"}
	address := resolver
	if _, _, err := net.SplitHostPort(resolver); err != nil {
		address = net.JoinHostPort(resolver, "53")
	}
	for _, recordType := range []uint16{dns.TypeA, dns.TypeAAAA} {
		message := new(dns.Msg)
		message.SetQuestion(dns.Fqdn(host), recordType)
		message.SetEdns0(1232, false)
		client := &dns.Client{Net: "udp", Timeout: 3 * time.Second}
		response, _, err := client.ExchangeContext(ctx, message, address)
		if err != nil {
			row.Status, row.Error = dnsLookupError(err)
			return row
		}
		if response.Truncated {
			client.Net = "tcp"
			response, _, err = client.ExchangeContext(ctx, message, address)
			if err != nil {
				row.Status, row.Error = dnsLookupError(err)
				return row
			}
		}
		if response.Rcode != dns.RcodeSuccess {
			row.Status = dns.RcodeToString[response.Rcode]
			row.Error = row.Status
			return row
		}
		readDNSAnswers(&row, response, host)
	}
	sort.Strings(row.IPv4)
	sort.Strings(row.IPv6)
	return row
}

func readDNSAnswers(row *dnsCompareRow, response *dns.Msg, host string) {
	for _, answer := range response.Answer {
		switch record := answer.(type) {
		case *dns.A:
			row.IPv4 = append(row.IPv4, record.A.String())
		case *dns.AAAA:
			row.IPv6 = append(row.IPv6, record.AAAA.String())
		case *dns.CNAME:
			row.CNAME = normalizeCNAME(record.Target, host)
		}
		ttl := answer.Header().Ttl
		if row.TTLSeconds == nil || ttl < *row.TTLSeconds {
			row.TTLSeconds = &ttl
		}
	}
}

func dnsLookupError(err error) (string, string) {
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) && dnsError.IsNotFound {
		return "NXDOMAIN", err.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", err.Error()
	}
	return "error", err.Error()
}

func normalizeCNAME(cname, host string) string {
	if strings.EqualFold(strings.TrimSuffix(cname, "."), strings.TrimSuffix(host, ".")) {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(cname), ".")
}

func dnsRowSignature(row dnsCompareRow) string {
	return row.Status + "|" + strings.Join(row.IPv4, ",") + "|" + strings.Join(row.IPv6, ",") + "|" + row.CNAME
}

func dnsCompareTable(rows []dnsCompareRow) string {
	var table strings.Builder
	for _, row := range rows {
		ttl := "—"
		if row.TTLSeconds != nil {
			ttl = fmt.Sprintf("%ds", *row.TTLSeconds)
		}
		fmt.Fprintf(&table, "%s  %s  TTL %s\n", row.Resolver, row.Status, ttl)
		fmt.Fprintf(&table, "  A %s\n  AAAA %s\n", strings.Join(row.IPv4, ", "), strings.Join(row.IPv6, ", "))
		if row.CNAME != "" {
			fmt.Fprintf(&table, "  CNAME %s\n", row.CNAME)
		}
		if row.Error != "" && row.Status != "NXDOMAIN" {
			fmt.Fprintf(&table, "  %s\n", firstLine(row.Error))
		}
	}
	return strings.TrimRight(table.String(), "\n")
}
