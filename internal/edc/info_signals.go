package edc

import (
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// infoSignals는 자원의 양이 아니라 장애의 원인이 되곤 하는 상태다. 모두 파일, syscall, sysctl로만 읽는다.
// 외부 명령을 띄우면 info가 다시 느려진다. 읽지 못한 값은 비워 두고 그 줄을 출력하지 않는다.
type infoSignals struct {
	// Platform은 하드웨어나 VM의 이름이다. Virtual은 hypervisor를 확인했을 때만 채운다.
	Platform, Virtual, Container string
	Clock                        *infoClock
	DNS                          infoDNS
	OOMKills                     *uint64
	// Pressure는 PSI some avg10의 CPU, memory, I/O 순서다. 없으면 nil이다.
	Pressure []float64
	Disks    map[string]diskSignal

	// 아래는 -v에서만 읽는다.
	Interfaces  []infoInterface
	IPv6Default string
	FileHandles *infoFileHandles
}

type infoInterface struct {
	Name string
	MTU  int
	MAC  string
	IPv6 []string
	// Link는 Linux가 알려 주는 속도와 duplex다. 가상 장치와 macOS는 비어 있다.
	Link string
}

type infoFileHandles struct{ Used, Max uint64 }

// infoClock은 커널의 동기화 표시만 담는다. adjtimex의 오차 값은 쓰지 않는다. maxerror는 마지막 갱신 뒤 늘어나기만 하는
// 상한이라 실제 오차가 0.7 ms인 OCI에서 2273 ms를 보였고, esterror는 systemd-timesyncd가 갱신하지 않아 0이었다.
type infoClock struct {
	Synced bool
}

type infoDNS struct {
	Config resolvConfig
	// Source는 resolv.conf가 local stub을 가리킬 때 실제 서버를 읽어 온 곳이다.
	Source string
	Err    error
}

type diskSignal struct {
	ReadOnly                bool
	InodesUsed, InodesTotal uint64
	// Options는 -v에서 보여 주는 마운트 옵션이다.
	Options string
}

// infoInodeWarnPercent를 넘으면 inode 사용률을 보여 준다. inode가 바닥나면 용량이 남아도 파일을 만들지 못한다.
const infoInodeWarnPercent = 80

// infoDNSShown은 기본 출력에 적는 nameserver의 수다. 나머지는 개수만 적는다.
const infoDNSShown = 3

// systemdResolvedStub은 systemd-resolved가 resolv.conf에 적는 local 주소다. 실제 서버는 따로 읽어야 보인다.
const systemdResolvedStub = "127.0.0.53"

func readInfoDNS(root string) infoDNS {
	data, err := os.ReadFile(filepath.Join(root, "etc/resolv.conf"))
	if err != nil {
		return infoDNS{Err: err}
	}
	dns := infoDNS{Config: parseResolvConf(string(data))}
	if !slices.Equal(dns.Config.Nameservers, []string{systemdResolvedStub}) {
		return dns
	}
	upstream, err := os.ReadFile(filepath.Join(root, "run/systemd/resolve/resolv.conf"))
	if err != nil {
		return dns
	}
	config := parseResolvConf(string(upstream))
	if len(config.Nameservers) == 0 {
		return dns
	}
	// search domain은 stub 쪽 파일에도 같은 값이 있다. 서버만 바꾼다.
	dns.Config.Nameservers, dns.Source = config.Nameservers, "systemd-resolved"
	return dns
}

// collectInfoInterfaces는 주소를 가진 인터페이스의 MTU, MAC, IPv6 주소를 읽는다. 기본 출력의 Local IP는 IPv4만 보여 준다.
func collectInfoInterfaces(link func(name string) string) []infoInterface {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var result []infoInterface
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		item := infoInterface{Name: iface.Name, MTU: iface.MTU, MAC: iface.HardwareAddr.String()}
		routable := false
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			ipnet, ok := address.(*net.IPNet)
			if !ok {
				continue
			}
			if ipnet.IP.To4() != nil {
				routable = true
				continue
			}
			item.IPv6 = append(item.IPv6, ipnet.String())
			routable = routable || !ipnet.IP.IsLinkLocalUnicast()
		}
		// 링크 로컬 IPv6만 가진 장치는 모든 인터페이스가 자동으로 받는 주소뿐이다. macOS에는 이런 가상 장치가 수십 개다.
		if !routable {
			continue
		}
		item.Link = link(iface.Name)
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func infoInterfaceLine(iface infoInterface) string {
	parts := []string{fmt.Sprintf("mtu %d", iface.MTU)}
	if iface.Link != "" {
		parts = append(parts, iface.Link)
	}
	if iface.MAC != "" {
		parts = append(parts, "MAC "+iface.MAC)
	}
	if len(iface.IPv6) > 0 {
		parts = append(parts, "IPv6 "+strings.Join(iface.IPv6, ", "))
	}
	return fmt.Sprintf("%-15s: %s", iface.Name, strings.Join(parts, " · "))
}

func infoFileHandlesLine(handles *infoFileHandles) string {
	if handles == nil {
		return ""
	}
	// 최근 Linux는 fs.file-max를 LONG_MAX로 두어 사실상 한도가 없다. 그 비율은 늘 0%라 뜻이 없다.
	if handles.Max >= math.MaxInt64 {
		return fmt.Sprintf("%d / unlimited", handles.Used)
	}
	line := fmt.Sprintf("%d / %d", handles.Used, handles.Max)
	if handles.Max > 0 {
		line += fmt.Sprintf(" (%.2f%%)", float64(handles.Used)/float64(handles.Max)*100)
	}
	return line
}

func infoPlatformLine(signals infoSignals) string {
	var parts []string
	for _, value := range []string{signals.Platform, signals.Virtual} {
		if value != "" {
			parts = append(parts, value)
		}
	}
	if signals.Container != "" {
		parts = append(parts, "container: "+signals.Container)
	}
	return strings.Join(parts, " · ")
}

func infoClockLine(clock *infoClock) string {
	if clock == nil {
		return ""
	}
	if !clock.Synced {
		return "not synchronized (kernel reports STA_UNSYNC)"
	}
	return "synchronized"
}

func infoDNSLine(dns infoDNS) string {
	if dns.Err != nil {
		return "unavailable: " + infoSingleLine(dns.Err.Error())
	}
	if len(dns.Config.Nameservers) == 0 {
		return "no nameserver in /etc/resolv.conf"
	}
	shown := dns.Config.Nameservers[:min(len(dns.Config.Nameservers), infoDNSShown)]
	line := strings.Join(shown, ", ")
	if hidden := len(dns.Config.Nameservers) - len(shown); hidden > 0 {
		line += fmt.Sprintf(" (+%d)", hidden)
	}
	if dns.Source != "" {
		line += " via " + dns.Source
	}
	if len(dns.Config.Search) > 0 {
		line += " · search " + strings.Join(dns.Config.Search, " ")
	}
	return line
}

func infoPressureLine(signals infoSignals) string {
	var parts []string
	if len(signals.Pressure) == 3 {
		parts = append(parts, fmt.Sprintf("Pressure (some avg10): CPU %.2f%% · Memory %.2f%% · I/O %.2f%%", signals.Pressure[0], signals.Pressure[1], signals.Pressure[2]))
	}
	if signals.OOMKills != nil {
		parts = append(parts, fmt.Sprintf("OOM kills since boot: %d", *signals.OOMKills))
	}
	return strings.Join(parts, " · ")
}

// diskSignalSuffix는 디스크 줄 끝에 붙일 경고다. 정상이면 빈 문자열이다.
func diskSignalSuffix(signal diskSignal) string {
	var parts []string
	if signal.ReadOnly {
		parts = append(parts, "read-only")
	}
	if signal.InodesTotal > 0 {
		percent := float64(signal.InodesUsed) / float64(signal.InodesTotal) * 100
		if percent >= infoInodeWarnPercent {
			parts = append(parts, fmt.Sprintf("inodes %.0f%%", percent))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "  (" + strings.Join(parts, " · ") + ")"
}
