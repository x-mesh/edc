package edc

import (
	"strings"
	"testing"
	"time"
)

func TestSelectRegionEndpoints(t *testing.T) {
	all, err := selectRegionEndpoints("all")
	if err != nil || len(all) != len(regionEndpoints) {
		t.Fatalf("all returned %d endpoints, err %v", len(all), err)
	}
	aws, err := selectRegionEndpoints("AWS")
	if err != nil {
		t.Fatalf("provider name must ignore case: %v", err)
	}
	for _, endpoint := range aws {
		if endpoint.provider != "aws" {
			t.Fatalf("aws selection holds %q", endpoint.provider)
		}
	}
	if len(aws) == 0 || len(aws) == len(all) {
		t.Fatalf("aws selection = %d of %d", len(aws), len(all))
	}
	if _, err := selectRegionEndpoints("azure"); err == nil {
		t.Fatal("an unknown provider must fail")
	}
}

func TestRegionEndpointTableIsUsable(t *testing.T) {
	seen := map[string]bool{}
	providers := map[string]int{}
	for _, endpoint := range regionEndpoints {
		if endpoint.city == "" || endpoint.provider == "" || endpoint.region == "" || endpoint.host == "" {
			t.Fatalf("incomplete endpoint: %#v", endpoint)
		}
		if seen[endpoint.host] {
			t.Fatalf("duplicate host: %s", endpoint.host)
		}
		seen[endpoint.host] = true
		providers[endpoint.provider]++
	}
	if len(providers) < 2 {
		t.Fatalf("the table must mix providers, got %v", providers)
	}
}

func TestParseCloudflareTrace(t *testing.T) {
	pop, loc := parseCloudflareTrace("fl=123\nip=203.0.113.7\ncolo=ICN\nloc=KR\n")
	if pop != "ICN" || loc != "KR" {
		t.Fatalf("pop = %q, loc = %q", pop, loc)
	}
	if pop, loc := parseCloudflareTrace("nothing useful"); pop != "" || loc != "" {
		t.Fatalf("unparsable body returned %q %q", pop, loc)
	}
}

func TestNetworkShapeDetection(t *testing.T) {
	if !isTunnelInterface("utun4") || !isTunnelInterface("wg0") {
		t.Error("tunnel interfaces must match")
	}
	if isTunnelInterface("en0") {
		t.Error("en0 is not a tunnel")
	}
	if !isPrivateAddress("192.168.1.10") || !isPrivateAddress("10.0.0.3") {
		t.Error("RFC 1918 addresses are private")
	}
	if isPrivateAddress("203.0.113.7") || isPrivateAddress("") {
		t.Error("a public address is not private")
	}
	if !isCarrierGradeNAT("100.64.0.1") || !isCarrierGradeNAT("100.127.255.254") {
		t.Error("100.64.0.0/10 is carrier-grade NAT")
	}
	if isCarrierGradeNAT("100.128.0.1") || isCarrierGradeNAT("192.168.1.1") {
		t.Error("addresses outside the block must not match")
	}
}

func TestWhereNetworkShapeReportsOnlyFacts(t *testing.T) {
	shape := whereNetworkShape(whereLocation{PublicIP: "203.0.113.7", LocalAddress: "192.168.1.10", BehindNAT: true, Tunnel: true})
	if !strings.Contains(shape, "NAT 뒤") || !strings.Contains(shape, "tunnel") {
		t.Fatalf("shape = %q", shape)
	}
	cgnat := whereNetworkShape(whereLocation{PublicIP: "100.64.0.1", CarrierGradeNAT: true, BehindNAT: true})
	if !strings.Contains(cgnat, "carrier-grade") {
		t.Fatalf("carrier-grade NAT must win over plain NAT: %q", cgnat)
	}
	if shape := whereNetworkShape(whereLocation{}); shape != "" {
		t.Fatalf("an empty location must claim nothing, got %q", shape)
	}
}

func TestWhereGeographyDropsRepeatedName(t *testing.T) {
	got := whereGeography(whereLocation{Country: "KR", Region: "Seoul", City: "Seoul"})
	if got != "KR  ·  Seoul" {
		t.Fatalf("geography = %q", got)
	}
	got = whereGeography(whereLocation{Country: "US", Region: "Oregon", City: "Portland"})
	if got != "US  ·  Oregon  ·  Portland" {
		t.Fatalf("geography = %q", got)
	}
}

func TestGroupByCityKeepsTheFastestProvider(t *testing.T) {
	measurements := []whereMeasurement{
		{City: "Tokyo", Provider: "aws", Min: 30 * time.Millisecond},
		{City: "Seoul", Provider: "aws", Min: 14 * time.Millisecond},
		{City: "Seoul", Provider: "gcp", Min: 6 * time.Millisecond},
		{City: "Paris", Provider: "aws", Error: "연결하지 못했습니다"},
	}
	rows := groupByCity(measurements)
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].city != "Seoul" || rows[0].best.Provider != "gcp" {
		t.Fatalf("first row = %#v", rows[0])
	}
	if len(rows[0].all) != 2 {
		t.Fatalf("Seoul must keep both providers, got %d", len(rows[0].all))
	}
	if rows[1].city != "Tokyo" {
		t.Fatalf("second row = %s", rows[1].city)
	}
	// 닿지 않은 도시는 언제나 마지막이다.
	if rows[2].city != "Paris" || rows[2].reached {
		t.Fatalf("last row = %#v", rows[2])
	}
}

func TestWhereConclusion(t *testing.T) {
	none := whereConclusion(nil, whereLocation{})
	if !strings.Contains(none, "닿지 못했습니다") {
		t.Fatalf("empty result = %q", none)
	}

	single := whereConclusion([]cityRow{{city: "Seoul", reached: true}}, whereLocation{})
	if !strings.Contains(single, "Seoul") {
		t.Fatalf("single result = %q", single)
	}

	clear := whereConclusion([]cityRow{
		{city: "Seoul", reached: true, best: whereMeasurement{Min: 6 * time.Millisecond}},
		{city: "Tokyo", reached: true, best: whereMeasurement{Min: 40 * time.Millisecond}},
	}, whereLocation{})
	if !strings.Contains(clear, "Seoul입니다") || !strings.Contains(clear, "34.0ms") {
		t.Fatalf("clear winner = %q", clear)
	}

	// 차이가 작으면 순위를 단정하지 않는다.
	close := whereConclusion([]cityRow{
		{city: "Seoul", reached: true, best: whereMeasurement{Min: 6 * time.Millisecond}},
		{city: "Tokyo", reached: true, best: whereMeasurement{Min: 8 * time.Millisecond}},
	}, whereLocation{})
	if !strings.Contains(close, "순위가 바뀔 수 있습니다") {
		t.Fatalf("close race = %q", close)
	}
}

func TestPadRespectsWideCharacters(t *testing.T) {
	// 한글 한 글자는 두 칸이므로 "최소"는 네 칸이다.
	if got := padLeft("최소", 9); liveWidth(got) != 9 {
		t.Fatalf("padLeft width = %d for %q", liveWidth(got), got)
	}
	if got := padRight("경로", 12); liveWidth(got) != 12 {
		t.Fatalf("padRight width = %d for %q", liveWidth(got), got)
	}
	if got := padRight("Cloudflare", 12); got != "Cloudflare  " {
		t.Fatalf("padRight = %q", got)
	}
	// 이미 넓으면 자르지 않는다.
	if got := padRight("verylongvalue", 4); got != "verylongvalue" {
		t.Fatalf("padRight truncated: %q", got)
	}
}

func TestWhereExitCodeFailsOnlyWhenNothingIsReached(t *testing.T) {
	if code := whereExitCode([]whereMeasurement{{Error: "x"}, {City: "Seoul"}}); code != 0 {
		t.Fatalf("one success must give 0, got %d", code)
	}
	if code := whereExitCode([]whereMeasurement{{Error: "x"}, {Error: "y"}}); code != 1 {
		t.Fatalf("no success must give 1, got %d", code)
	}
}

func TestWhereProgressModelShowsCountAndElapsed(t *testing.T) {
	model := newWhereProgressModel(23, nil)
	model.now = func() time.Time { return model.started.Add(1400 * time.Millisecond) }

	view := model.View().Content
	if !strings.Contains(view, "0/23") || !strings.Contains(view, "1.4s") {
		t.Fatalf("initial view = %q", view)
	}
	if liveLineCount(view) != 1 {
		t.Fatalf("the progress line must stay on one line: %q", view)
	}

	advanced, _ := model.Update(whereProgressMsg{done: 7})
	moved := advanced.(whereProgressModel)
	moved.now = model.now
	if got := moved.View().Content; !strings.Contains(got, "7/23") {
		t.Fatalf("view after progress = %q", got)
	}
}

func TestWhereProgressModelClearsItselfWhenDone(t *testing.T) {
	model := newWhereProgressModel(4, nil)
	finished, cmd := model.Update(whereFinishedMsg{})
	done := finished.(whereProgressModel)
	if !done.stopped || cmd == nil {
		t.Fatalf("model = %#v, cmd = %v", done, cmd)
	}
	// 진행 줄이 결과 위에 남으면 안 된다.
	if content := strings.TrimSpace(done.View().Content); content != "" {
		t.Fatalf("finished view must be blank, got %q", content)
	}
}

func TestRedactWhereLocationHidesEveryAddress(t *testing.T) {
	location := redactWhereLocation(whereLocation{
		PublicIP: "203.0.113.7", LocalAddress: "192.168.1.10", Gateway: "192.168.1.1", Org: "AS64500 Example",
	})
	for _, value := range []string{location.PublicIP, location.LocalAddress, location.Gateway} {
		if !strings.HasPrefix(value, "<ip:") {
			t.Fatalf("address stayed visible: %q", value)
		}
	}
	if location.Org != "AS64500 Example" {
		t.Fatalf("the ASN is not an address: %q", location.Org)
	}
}
