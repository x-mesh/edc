package edc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

const ripeStatURL = "https://stat.ripe.net"
const ownerResponseLimit = 1 << 20

type endpointNetwork struct {
	Prefix  string   `json:"prefix,omitempty"`
	ASNs    []string `json:"asns,omitempty"`
	Holders []string `json:"holders,omitempty"`
	Scope   string   `json:"scope,omitempty"`
	Error   string   `json:"error,omitempty"`
}

type endpointOwnerResolver struct {
	client  *http.Client
	baseURL string
	holders sync.Map
}

func newEndpointOwnerResolver(baseURL string) *endpointOwnerResolver {
	return &endpointOwnerResolver{client: &http.Client{Timeout: 3 * time.Second}, baseURL: baseURL}
}

func (resolver *endpointOwnerResolver) lookup(ctx context.Context, ip string) endpointNetwork {
	address, err := netip.ParseAddr(ip)
	if err != nil {
		return endpointNetwork{Error: err.Error()}
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() {
		return endpointNetwork{Scope: "private"}
	}
	var network struct {
		Status string `json:"status"`
		Data   struct {
			ASNs   []string `json:"asns"`
			Prefix string   `json:"prefix"`
		} `json:"data"`
	}
	if err := resolver.get(ctx, "/data/network-info/data.json", ip, &network); err != nil {
		return endpointNetwork{Error: err.Error()}
	}
	info := endpointNetwork{Prefix: network.Data.Prefix, ASNs: network.Data.ASNs}
	for _, asn := range info.ASNs {
		holder, err := resolver.holder(ctx, asn)
		if err != nil {
			info.Error = err.Error()
			continue
		}
		if holder != "" {
			info.Holders = append(info.Holders, holder)
		}
	}
	return info
}

func (resolver *endpointOwnerResolver) holder(ctx context.Context, asn string) (string, error) {
	value, _ := resolver.holders.LoadOrStore(asn, sync.OnceValues(func() (string, error) {
		var overview struct {
			Status string `json:"status"`
			Data   struct {
				Holder string `json:"holder"`
			} `json:"data"`
		}
		if err := resolver.get(ctx, "/data/as-overview/data.json", "AS"+asn, &overview); err != nil {
			return "", err
		}
		return overview.Data.Holder, nil
	}))
	return value.(func() (string, error))()
}

func (resolver *endpointOwnerResolver) get(ctx context.Context, path, resource string, destination interface{}) error {
	query := url.Values{"resource": {resource}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, resolver.baseURL+path+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	response, err := resolver.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("RIPEstat HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, ownerResponseLimit+1))
	if err != nil {
		return err
	}
	if len(data) > ownerResponseLimit {
		return errors.New("RIPEstat response is too large")
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return err
	}
	var envelope struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if envelope.Status != "ok" {
		return fmt.Errorf("RIPEstat status %q", envelope.Status)
	}
	return nil
}

func endpointOwnerLabel(network endpointNetwork) string {
	if network.Scope == "private" {
		return T("observe.probe.owner_private")
	}
	if len(network.Holders) == 0 {
		if network.Error != "" {
			return T("observe.probe.owner_unavailable")
		}
		return T("observe.probe.owner_unknown")
	}
	owners := make([]string, 0, len(network.Holders))
	for _, holder := range network.Holders {
		if _, name, found := strings.Cut(holder, " - "); found {
			holder = name
		}
		owners = append(owners, holder)
	}
	return strings.Join(owners, "; ")
}
