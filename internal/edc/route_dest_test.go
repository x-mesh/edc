package edc

import (
	"net"
	"testing"
)

// netlink은 IPv4 기본 경로의 Dst를 nil로 줄 때도 있고 0.0.0.0/0으로 줄 때도 있다. 후자를 default로
// 보지 않으면 entriesForDest가 기본 경로를 못 찾아 lockout과 target 판정이 통째로 실패한다.
func TestRouteDestNameRecognizesEveryDefaultForm(t *testing.T) {
	cases := []struct {
		name string
		dst  *net.IPNet
		want string
	}{
		{"nil", nil, "default"},
		{"0.0.0.0/0", &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}, "default"},
		{"::/0", &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}, "default"},
		{"호스트 경로는 주소만", &net.IPNet{IP: net.ParseIP("8.8.8.8").To4(), Mask: net.CIDRMask(32, 32)}, "8.8.8.8"},
		{"prefix는 그대로", &net.IPNet{IP: net.ParseIP("192.0.2.0").To4(), Mask: net.CIDRMask(24, 32)}, "192.0.2.0/24"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := routeDestName(testCase.dst); got != testCase.want {
				t.Fatalf("routeDestName = %q, want %q", got, testCase.want)
			}
		})
	}
}
