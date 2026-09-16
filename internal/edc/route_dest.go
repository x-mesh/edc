package edc

import (
	"fmt"
	"net"
	"strings"
)

// routeDestName은 netlink이 준 목적지를 ip route show와 같은 표기로 바꾼다. exits.yaml의 dest나
// 사용자가 적은 값과 맞추려면 표기가 같아야 한다.
//
// 기본 경로는 두 형태로 온다. Dst가 nil인 경우와 0.0.0.0/0(또는 ::/0)인 경우다. 둘 다 default로
// 보지 않으면 entriesForDest가 기본 경로를 찾지 못한다.
func routeDestName(dst *net.IPNet) string {
	if dst == nil {
		return "default"
	}
	ones, bits := dst.Mask.Size()
	if ones == 0 && dst.IP.IsUnspecified() {
		return "default"
	}
	if bits > 0 && ones == bits {
		return dst.IP.String()
	}
	return dst.String()
}

// parseRouteDest는 routeDestName의 역방향이다. 호스트 주소만 적힌 경우 전체 길이 mask를 붙인다.
func parseRouteDest(dest string) (*net.IPNet, error) {
	if strings.Contains(dest, "/") {
		_, network, err := net.ParseCIDR(dest)
		if err != nil {
			return nil, err
		}
		return network, nil
	}
	ip := net.ParseIP(dest)
	if ip == nil {
		return nil, fmt.Errorf("not an address or prefix: %s", dest)
	}
	if v4 := ip.To4(); v4 != nil {
		return &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)}, nil
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}, nil
}
