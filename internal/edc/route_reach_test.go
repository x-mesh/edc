package edc

import "testing"

// neighborFixture는 실제 Ubuntu 24.04 호스트의 `ip neigh show` 출력 형태다. 주소만 문서용 대역으로
// 바꿨다. STALE은 lladdr이 있어 쓸 수 있고, INCOMPLETE와 FAILED는 lladdr이 없어 확정 실패다.
const neighborFixture = `192.0.2.1 dev enp1s0 lladdr 00:00:5e:00:53:01 REACHABLE
192.0.2.2 dev enp1s0 lladdr 00:00:5e:00:53:02 STALE
192.0.2.253 dev enp1s0 INCOMPLETE
192.0.2.252 dev enp1s0 FAILED
192.0.2.254 dev enp1s0 lladdr 02:00:00:00:00:01 PERMANENT
`

// linkFixture는 실제 호스트의 `ip -o link show` 출력이다. docker0은 UP이지만 NO-CARRIER이고,
// tailscale0은 정상 동작 중인데도 state가 UNKNOWN이다. edctest0은 UP flag 자체가 없다.
const linkFixture = `1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN mode DEFAULT group default qlen 1000\    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
2: enp1s0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc fq_codel state UP mode DEFAULT group default qlen 1000\    link/ether 00:00:5e:00:53:03 brd ff:ff:ff:ff:ff:ff
3: docker0: <NO-CARRIER,BROADCAST,MULTICAST,UP> mtu 1500 qdisc noqueue state DOWN mode DEFAULT group default \    link/ether 00:00:5e:00:53:04 brd ff:ff:ff:ff:ff:ff
4: tailscale0: <POINTOPOINT,MULTICAST,NOARP,UP,LOWER_UP> mtu 1280 qdisc fq_codel state UNKNOWN mode DEFAULT group default qlen 500\    link/none
5: edctest0: <BROADCAST,NOARP> mtu 1500 qdisc noop state DOWN mode DEFAULT group default qlen 1000\    link/ether 00:00:5e:00:53:05 brd ff:ff:ff:ff:ff:ff
`

func TestParseNeighbors(t *testing.T) {
	entries := parseNeighbors(neighborFixture)
	if len(entries) != 5 {
		t.Fatalf("entries = %d, want 5: %#v", len(entries), entries)
	}
	if entries[0].Dest != "192.0.2.1" || entries[0].Dev != "enp1s0" || entries[0].LLAddr != "00:00:5e:00:53:01" || entries[0].State != "REACHABLE" {
		t.Fatalf("entry 0 = %#v", entries[0])
	}
	// lladdr 없는 줄도 dev와 state를 읽어야 확정 실패로 판정할 수 있다.
	if entries[2].Dest != "192.0.2.253" || entries[2].LLAddr != "" || entries[2].State != "INCOMPLETE" {
		t.Fatalf("entry 2 = %#v", entries[2])
	}
}

// 항목이 없는 것과 실패한 것은 다르다. 아직 통신한 적 없는 next-hop은 항목 자체가 없고 ip neigh show가
// 빈 출력에 exit 0을 준다. 없음을 실패로 처리하면 멀쩡한 출구를 거부한다.
func TestNeighborReachSeparatesAbsentFromFailed(t *testing.T) {
	entries := parseNeighbors(neighborFixture)
	cases := []struct {
		dest string
		want string
	}{
		{"192.0.2.1", reachUsable},   // REACHABLE
		{"192.0.2.2", reachUsable},   // STALE이어도 lladdr이 있으면 전달된다
		{"192.0.2.254", reachUsable}, // PERMANENT: 가짜 MAC이어도 L2에서는 구분되지 않는다
		{"192.0.2.253", reachBroken}, // INCOMPLETE
		{"192.0.2.252", reachBroken}, // FAILED
		{"192.0.2.99", reachUnknown}, // 항목 없음
	}
	for _, testCase := range cases {
		if state, _ := neighborReach(entries, testCase.dest); state != testCase.want {
			t.Fatalf("neighborReach(%s) = %q, want %q", testCase.dest, state, testCase.want)
		}
	}
}

func TestParseLinksReadsMTUAndFlags(t *testing.T) {
	links := parseLinks(linkFixture)
	if len(links) != 5 {
		t.Fatalf("links = %d, want 5: %#v", len(links), links)
	}
	byName := map[string]linkInfo{}
	for _, link := range links {
		byName[link.Name] = link
	}
	if byName["enp1s0"].MTU != 1500 || byName["enp1s0"].State != "UP" {
		t.Fatalf("enp1s0 = %#v", byName["enp1s0"])
	}
	// 터널 MTU는 VXLAN 오버헤드 진단의 입력이다.
	if byName["tailscale0"].MTU != 1280 {
		t.Fatalf("tailscale0 mtu = %d, want 1280", byName["tailscale0"].MTU)
	}
	if byName["lo"].MTU != 65536 {
		t.Fatalf("lo mtu = %d, want 65536", byName["lo"].MTU)
	}
}

// state가 아니라 flag로 판정해야 한다. tailscale0와 lo는 정상 동작 중에도 state가 UNKNOWN이다.
func TestLinkReachUsesFlagsNotState(t *testing.T) {
	links := parseLinks(linkFixture)
	cases := []struct {
		name string
		want string
	}{
		{"enp1s0", reachUsable},
		{"tailscale0", reachUsable}, // state UNKNOWN이지만 UP flag가 있다
		{"lo", reachUsable},         // 같은 이유
		{"docker0", reachBroken},    // UP이지만 NO-CARRIER
		{"edctest0", reachBroken},   // UP flag 자체가 없다
		{"missing0", reachUnknown},
	}
	for _, testCase := range cases {
		if state, _, _ := linkReach(links, testCase.name); state != testCase.want {
			t.Fatalf("linkReach(%s) = %q, want %q", testCase.name, state, testCase.want)
		}
	}
}

func TestExitReachCombinesNeighborAndLink(t *testing.T) {
	neighbors := parseNeighbors(neighborFixture)
	links := parseLinks(linkFixture)
	cases := []struct {
		name string
		via  string
		dev  string
		want string
	}{
		{"정상 출구", "192.0.2.1", "enp1s0", reachUsable},
		{"죽은 next-hop", "192.0.2.253", "enp1s0", reachBroken},
		{"캐리어 없는 장치", "192.0.2.1", "docker0", reachBroken},
		{"둘 다 실패", "192.0.2.252", "edctest0", reachBroken},
		{"미확인 next-hop", "192.0.2.99", "enp1s0", reachUnknown},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			state, _, _, mtu := exitReach(neighbors, links, testCase.via, testCase.dev)
			if state != testCase.want {
				t.Fatalf("exitReach(%s, %s) = %q, want %q", testCase.via, testCase.dev, state, testCase.want)
			}
			if testCase.dev == "enp1s0" && mtu != 1500 {
				t.Fatalf("mtu = %d, want 1500", mtu)
			}
		})
	}
}
