//go:build linux

package edc

import (
	"bufio"
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// dnsRecordType은 BPF가 받은 DNS 응답을 넘기는 내부 레코드다. 사용자에게 보일 event가 아니다.
const (
	dnsRecordType          = 9
	dnsRecordPayloadOffset = 24
)

const (
	targetSourceCommand       = "command"
	targetSourceDNS           = "dns"
	targetSourceResolverCache = "resolver-cache"
)

// dnsNameLimit은 이름이 계속 쌓여도 메모리를 제한한다. BPF의 socket 주인 map과 같은 크기다.
const dnsNameLimit = 65536

type dnsProcessAddress struct {
	pid     uint32
	address netip.Addr
}

type dnsName struct {
	name   string
	source string
}

// dnsNameCache는 DNS 응답과 resolver 캐시에서 본 주소의 이름이다. 프로세스가 직접 조회한 이름을 따로 둔다.
// CDN처럼 한 주소를 여러 이름이 함께 쓰면 주소별 이름은 다른 프로세스의 조회일 수 있다.
type dnsNameCache struct {
	byProcess map[dnsProcessAddress]string
	byAddress map[netip.Addr]dnsName
}

func newDNSNameCache() *dnsNameCache {
	return &dnsNameCache{byProcess: map[dnsProcessAddress]string{}, byAddress: map[netip.Addr]dnsName{}}
}

func (cache *dnsNameCache) rememberAnswer(pid uint32, names map[netip.Addr]string) {
	for address, name := range names {
		key := dnsProcessAddress{pid: pid, address: address}
		if _, ok := cache.byProcess[key]; !ok && len(cache.byProcess) >= dnsNameLimit {
			clear(cache.byProcess)
		}
		cache.byProcess[key] = name
		cache.rememberAddress(address, name, targetSourceDNS)
	}
}

func (cache *dnsNameCache) rememberAddress(address netip.Addr, name, source string) {
	if _, ok := cache.byAddress[address]; !ok && len(cache.byAddress) >= dnsNameLimit {
		clear(cache.byAddress)
	}
	cache.byAddress[address] = dnsName{name: name, source: source}
}

func (cache *dnsNameCache) forProcess(pid uint32, destination string) (string, bool) {
	address, ok := traceDestinationAddress(destination)
	if !ok {
		return "", false
	}
	name, ok := cache.byProcess[dnsProcessAddress{pid: pid, address: address}]
	return name, ok
}

func (cache *dnsNameCache) forAddress(destination string) (dnsName, bool) {
	address, ok := traceDestinationAddress(destination)
	if !ok {
		return dnsName{}, false
	}
	name, ok := cache.byAddress[address]
	return name, ok
}

// traceDestinationAddress는 event의 host:port에서 주소를 꺼낸다. event는 IPv6를 축약하지 않고 쓰고 DNS는
// 축약해서 쓰므로 netip로 같은 값으로 맞춘다.
func traceDestinationAddress(destination string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(destination)
	if err != nil {
		return netip.Addr{}, false
	}
	address, err := netip.ParseAddr(host)
	if err != nil || address.IsUnspecified() {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
}

func parseDNSRecord(sample []byte) (uint32, []byte, bool) {
	if len(sample) < dnsRecordPayloadOffset || binary.LittleEndian.Uint32(sample[8:12]) != dnsRecordType {
		return 0, nil, false
	}
	pid := binary.LittleEndian.Uint32(sample[12:16])
	payload := sample[dnsRecordPayloadOffset:]
	if size := int(binary.LittleEndian.Uint32(sample[16:20])); size < len(payload) {
		payload = payload[:size]
	}
	return pid, payload, true
}

// dnsAnswerNames는 응답의 A와 AAAA 주소를 질의 이름에 연결한다. CNAME을 거쳐도 프로그램이 물어본 이름이
// 사용자에게 의미가 있다. 잘린 응답은 해석하지 않는다.
func dnsAnswerNames(payload []byte) map[netip.Addr]string {
	var message dns.Msg
	if err := message.Unpack(payload); err != nil || !message.Response || len(message.Question) == 0 {
		return nil
	}
	name := traceDNSName(message.Question[0].Name)
	names := map[netip.Addr]string{}
	for _, answer := range message.Answer {
		var ip net.IP
		switch record := answer.(type) {
		case *dns.A:
			ip = record.A
		case *dns.AAAA:
			ip = record.AAAA
		default:
			continue
		}
		if address, ok := netip.AddrFromSlice(ip); ok {
			names[address.Unmap()] = name
		}
	}
	return names
}

func traceDNSName(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

// seedResolverCache는 trace 전에 조회된 이름을 systemd-resolved 캐시에서 채운다. resolvectl이 없거나
// 실패하면 채우지 않는다. 그때는 trace 중에 본 DNS 응답과 명령줄만으로 이름을 정한다.
func seedResolverCache(cache *dnsNameCache) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "resolvectl", "show-cache").Output()
	if err != nil {
		return
	}
	for address, name := range resolverCacheNames(string(output)) {
		cache.rememberAddress(address, name, targetSourceResolverCache)
	}
}

// resolverCacheNames는 "name IN A 1.2.3.4" 형식의 줄을 읽는다. 주소는 CNAME을 거슬러 올라간 첫 이름에
// 연결한다. 여러 이름이 같은 곳을 가리키면 사전순으로 첫 이름을 쓴다.
func resolverCacheNames(output string) map[netip.Addr]string {
	aliases := map[string][]string{}
	owners := map[netip.Addr][]string{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 4 || fields[1] != "IN" {
			continue
		}
		name := traceDNSName(fields[0])
		switch fields[2] {
		case "CNAME":
			target := traceDNSName(fields[3])
			aliases[target] = append(aliases[target], name)
		case "A", "AAAA":
			if address, err := netip.ParseAddr(fields[3]); err == nil {
				owners[address.Unmap()] = append(owners[address.Unmap()], name)
			}
		}
	}
	names := map[netip.Addr]string{}
	for address, list := range owners {
		heads := []string{}
		for _, owner := range list {
			heads = append(heads, resolverAliasHeads(owner, aliases, map[string]bool{})...)
		}
		sort.Strings(heads)
		names[address] = heads[0]
	}
	return names
}

func resolverAliasHeads(name string, aliases map[string][]string, seen map[string]bool) []string {
	if seen[name] {
		return []string{name}
	}
	seen[name] = true
	parents := aliases[name]
	if len(parents) == 0 {
		return []string{name}
	}
	heads := []string{}
	for _, parent := range parents {
		heads = append(heads, resolverAliasHeads(parent, aliases, seen)...)
	}
	return heads
}

// resolveTraceTarget은 target과 그 출처를 정한다. 프로세스가 trace 중에 직접 조회한 이름이 가장 확실하고,
// 명령줄 인자는 파일 이름을 잘못 고를 수 있어 그다음이다. 주소별 이름은 다른 프로세스의 조회일 수 있어 마지막이다.
func resolveTraceTarget(event captureEvent, commandTarget string, names *dnsNameCache) (string, string) {
	if event.PID != 0 {
		if name, ok := names.forProcess(event.PID, event.Destination); ok {
			return name, targetSourceDNS
		}
	}
	if commandTarget != "" {
		return commandTarget, targetSourceCommand
	}
	if name, ok := names.forAddress(event.Destination); ok {
		return name.name, name.source
	}
	return "", ""
}
