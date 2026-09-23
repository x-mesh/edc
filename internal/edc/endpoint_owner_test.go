package edc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestEndpointOwnerLookupReusesASNHolder(t *testing.T) {
	var holderRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/data/network-info/data.json":
			fmt.Fprint(writer, `{"status":"ok","data":{"asns":["23576"],"prefix":"223.130.192.0/22"}}`)
		case "/data/as-overview/data.json":
			holderRequests.Add(1)
			fmt.Fprint(writer, `{"status":"ok","data":{"holder":"nhn-AS-KR-KR - NAVER Cloud Corp."}}`)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	resolver := newEndpointOwnerResolver(server.URL)
	addresses := []string{"223.130.192.247", "223.130.192.248"}
	results := make([]endpointNetwork, len(addresses))
	var workers sync.WaitGroup
	for index, ip := range addresses {
		workers.Add(1)
		go func() {
			defer workers.Done()
			results[index] = resolver.lookup(context.Background(), ip)
		}()
	}
	workers.Wait()
	for _, network := range results {
		if network.Prefix != "223.130.192.0/22" || len(network.ASNs) != 1 || network.ASNs[0] != "23576" || endpointOwnerLabel(network) != "NAVER Cloud Corp." {
			t.Fatalf("network = %#v", network)
		}
	}
	if got := holderRequests.Load(); got != 1 {
		t.Fatalf("ASN holder requests = %d", got)
	}
}

func TestEndpointOwnerLookupSkipsPrivateAddresses(t *testing.T) {
	resolver := newEndpointOwnerResolver("http://invalid.example")
	network := resolver.lookup(context.Background(), "127.0.0.1")
	if network.Scope != "private" || network.Error != "" {
		t.Fatalf("network = %#v", network)
	}
}

func TestEndpointOwnerLookupReportsServiceFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	network := newEndpointOwnerResolver(server.URL).lookup(context.Background(), "223.130.192.247")
	if network.Error == "" || endpointOwnerLabel(network) != T("observe.probe.owner_unavailable") {
		t.Fatalf("network = %#v", network)
	}
}

func TestEndpointOwnerFailureDoesNotFailEndpoint(t *testing.T) {
	check := endpointCheck{IP: "192.0.2.1", TCP: StatusPass, TLS: StatusPass, HTTP: StatusPass, Network: endpointNetwork{Error: "timeout"}}
	if endpointStatus(check) != StatusPass || !strings.Contains(endpointTable([]endpointCheck{check}), T("observe.probe.owner_unavailable")) {
		t.Fatalf("check = %#v", check)
	}
}
