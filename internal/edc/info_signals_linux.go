//go:build linux

package edc

import (
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func collectInfoSignals(verbose bool) infoSignals {
	signals := infoSignals{DNS: readInfoDNS("/")}
	signals.Platform, signals.Virtual = linuxPlatform("/")
	signals.Container = linuxContainer("/", os.Getenv("KUBERNETES_SERVICE_HOST") != "")
	signals.Clock = linuxClock()
	if data, err := os.ReadFile("/proc/vmstat"); err == nil {
		if value, ok := parseLinuxVMStatValue(string(data), "oom_kill"); ok {
			signals.OOMKills = &value
		}
	}
	var pressure []float64
	for _, resource := range []string{"cpu", "memory", "io"} {
		values, ok := readLinuxPressure("/proc/pressure/"+resource, "some")
		if !ok {
			pressure = nil
			break
		}
		pressure = append(pressure, values[0])
	}
	signals.Pressure = pressure
	if verbose {
		signals.Interfaces = collectInfoInterfaces(linuxLinkSpeed)
		if data, err := os.ReadFile("/proc/net/ipv6_route"); err == nil {
			signals.IPv6Default = parseLinuxIPv6Default(string(data))
		}
		if data, err := os.ReadFile("/proc/sys/fs/file-nr"); err == nil {
			signals.FileHandles = parseLinuxFileNr(string(data))
		}
	}
	return signals
}

// linuxLinkSpeed는 NIC가 알려 주는 속도와 duplex다. 가상 장치는 읽기가 실패하거나 -1을 준다.
func linuxLinkSpeed(name string) string {
	directory := filepath.Join("/sys/class/net", name)
	data, err := os.ReadFile(filepath.Join(directory, "speed"))
	if err != nil {
		return ""
	}
	speed, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || speed <= 0 {
		return ""
	}
	link := fmt.Sprintf("%d Mb/s", speed)
	if duplex, err := os.ReadFile(filepath.Join(directory, "duplex")); err == nil {
		if value := strings.TrimSpace(string(duplex)); value != "" && value != "unknown" {
			link += " " + value
		}
	}
	return link
}

const (
	linuxRouteUp      = 0x1   // RTF_UP
	linuxRouteGateway = 0x2   // RTF_GATEWAY
	linuxRouteReject  = 0x200 // RTF_REJECT
)

// parseLinuxIPv6Default는 /proc/net/ipv6_route에서 metric이 가장 낮은 기본 경로를 고른다. 열은 목적지, 길이, 출발지, 길이,
// next hop, metric, refcnt, use, flags, 장치 순서이고 주소는 32자리 16진수다.
func parseLinuxIPv6Default(input string) string {
	best, bestMetric := "", uint64(0)
	for _, line := range strings.Split(input, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || fields[0] != strings.Repeat("0", 32) || fields[1] != "00" || fields[9] == "lo" {
			continue
		}
		metric, err1 := strconv.ParseUint(fields[5], 16, 32)
		flags, err2 := strconv.ParseUint(fields[8], 16, 32)
		if err1 != nil || err2 != nil || flags&linuxRouteUp == 0 || flags&linuxRouteReject != 0 {
			continue
		}
		route := "dev " + fields[9]
		if flags&linuxRouteGateway != 0 {
			if raw, err := hex.DecodeString(fields[4]); err == nil && len(raw) == 16 {
				route = netip.AddrFrom16([16]byte(raw)).String() + " " + route
			}
		}
		if best == "" || metric < bestMetric {
			best, bestMetric = route, metric
		}
	}
	return best
}

// parseLinuxFileNr는 할당한 수, 쓰지 않는 수, 한도 순서인 file-nr에서 사용 중인 수와 한도를 읽는다.
func parseLinuxFileNr(input string) *infoFileHandles {
	fields := strings.Fields(input)
	if len(fields) < 3 {
		return nil
	}
	allocated, err1 := strconv.ParseUint(fields[0], 10, 64)
	unused, err2 := strconv.ParseUint(fields[1], 10, 64)
	limit, err3 := strconv.ParseUint(fields[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || unused > allocated {
		return nil
	}
	return &infoFileHandles{Used: allocated - unused, Max: limit}
}

func collectDiskSignals(disks []diskDetails, verbose bool) map[string]diskSignal {
	var options map[string]string
	if verbose {
		if data, err := os.ReadFile("/proc/self/mounts"); err == nil {
			options = parseLinuxMountOptions(string(data))
		}
	}
	signals := map[string]diskSignal{}
	deleted := scanDeletedOpen("/proc", linuxDeletedOpenStat, infoDeletedOpenFDLimit)
	for _, disk := range visibleDisks(disks) {
		if signal, ok := statfsDiskSignal(disk.Mount); ok {
			signal.Options = options[disk.Mount]
			if dev, ok := mountDevice(disk.Mount); ok && deleted[dev] != nil {
				signal.DeletedOpen = *deleted[dev]
			}
			signals[disk.Mount] = signal
		}
	}
	return signals
}

func linuxDeletedOpenStat(path string) (deletedOpenStat, error) {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return deletedOpenStat{}, err
	}
	size := stat.Size
	if size < 0 {
		size = 0
	}
	return deletedOpenStat{Dev: uint64(stat.Dev), Inode: stat.Ino, Size: uint64(size), Links: uint64(stat.Nlink), Regular: stat.Mode&unix.S_IFMT == unix.S_IFREG}, nil
}

// mountDevice는 마운트 지점의 st_dev다. 같은 파일 시스템의 파일은 같은 값을 가지므로 fd의 st_dev와 맞춘다.
func mountDevice(mount string) (uint64, bool) {
	var stat unix.Stat_t
	if err := unix.Stat(mount, &stat); err != nil {
		return 0, false
	}
	return uint64(stat.Dev), true
}

// parseLinuxMountOptions는 /proc/self/mounts에서 마운트 지점마다 옵션을 읽는다. 같은 곳에 겹쳐 붙이면 마지막 것이 보인다.
func parseLinuxMountOptions(input string) map[string]string {
	options := map[string]string{}
	for _, line := range strings.Split(input, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 {
			options[unescapeMountField(fields[1])] = fields[3]
		}
	}
	return options
}

// linuxCloudAssetTags는 제조사 이름에 클라우드가 드러나지 않는 곳의 chassis asset tag다. OCI는 sys_vendor를 QEMU로 둔다.
var linuxCloudAssetTags = map[string]string{"OracleCloud.com": "Oracle Cloud"}

// linuxPlatform은 DMI의 제조사와 제품 이름을 잇는다. 클라우드는 이 값에 서비스와 instance type을 적는다
// (예: Amazon EC2 m5.large). VM은 CPU의 hypervisor flag나 QEMU/KVM의 DMI 이름으로만 판단하고, 아니라고 bare metal로 단정하지 않는다.
// ARM은 cpuinfo에 hypervisor flag가 없어 DMI 이름이 유일한 단서다.
func linuxPlatform(root string) (platform, virtual string) {
	read := func(name string) string {
		data, _ := os.ReadFile(filepath.Join(root, "sys/class/dmi/id", name))
		return strings.TrimSpace(string(data))
	}
	vendor, product := read("sys_vendor"), read("product_name")
	switch {
	case product == "":
		platform = vendor
	case vendor == "" || strings.HasPrefix(product, vendor):
		platform = product
	default:
		platform = vendor + " " + product
	}
	if cloud := linuxCloudAssetTags[read("chassis_asset_tag")]; cloud != "" {
		platform = strings.TrimSuffix(cloud+" · "+platform, " · ")
	}
	isVirtual := vendor == "QEMU" || strings.Contains(product, "Virtual Machine")
	if cpuinfo, err := os.ReadFile(filepath.Join(root, "proc/cpuinfo")); err == nil && linuxCPUHasHypervisor(string(cpuinfo)) {
		isVirtual = true
	}
	// 제품 이름이 이미 VM이라고 말하면 되풀이하지 않는다.
	if isVirtual && !strings.Contains(strings.ToLower(platform), "virtual machine") {
		virtual = "virtual machine"
	}
	return platform, virtual
}

func linuxCPUHasHypervisor(cpuinfo string) bool {
	for _, line := range strings.Split(cpuinfo, "\n") {
		key, value, found := strings.Cut(line, ":")
		if found && strings.TrimSpace(key) == "flags" {
			return strings.Contains(" "+value+" ", " hypervisor ")
		}
	}
	return false
}

// linuxContainer는 runtime이 남기는 표식으로 container 안인지 본다. 표식이 없으면 비워 둔다.
func linuxContainer(root string, kubernetes bool) string {
	if kubernetes {
		return "kubernetes"
	}
	if _, err := os.Stat(filepath.Join(root, ".dockerenv")); err == nil {
		return "docker"
	}
	if _, err := os.Stat(filepath.Join(root, "run/.containerenv")); err == nil {
		return "podman"
	}
	return ""
}

// linuxClock은 adjtimex를 읽기만 한다. modes가 0이면 시계를 바꾸지 않아 권한이 필요 없다.
func linuxClock() *infoClock {
	var timex unix.Timex
	state, err := unix.Adjtimex(&timex)
	if err != nil {
		return nil
	}
	return &infoClock{Synced: timex.Status&unix.STA_UNSYNC == 0 && state != unix.TIME_ERROR}
}

func parseLinuxVMStatValue(input, name string) (uint64, bool) {
	for _, line := range strings.Split(input, "\n") {
		key, value, found := strings.Cut(line, " ")
		if found && key == name {
			parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			return parsed, err == nil
		}
	}
	return 0, false
}

func statfsDiskSignal(path string) (diskSignal, bool) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return diskSignal{}, false
	}
	signal := diskSignal{ReadOnly: stat.Flags&unix.ST_RDONLY != 0}
	// btrfs처럼 inode 수를 정하지 않는 파일 시스템은 0을 준다.
	if stat.Files > 0 && stat.Ffree <= stat.Files {
		signal.InodesUsed, signal.InodesTotal = stat.Files-stat.Ffree, stat.Files
	}
	return signal, true
}
