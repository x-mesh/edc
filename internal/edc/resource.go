package edc

import (
	"bufio"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

type resourceSnapshot struct {
	TakenAt     time.Time
	CPUUser     uint64
	CPUSystem   uint64
	CPUIOWait   uint64
	CPUIdle     uint64
	CPUTotal    uint64
	NetInBytes  uint64
	NetOutBytes uint64
	PacketsIn   uint64
	PacketsOut  uint64
	DiskRead    uint64
	DiskWrite   uint64
	MemoryUsed  uint64
	MemoryTotal uint64
	Load1       float64
	CPUInstant  bool
}

type resourceRate struct {
	NetIn, NetOut                 float64
	PacketsIn, PacketsOut         float64
	DiskRead, DiskWrite           float64
	CPUUser, CPUSystem, CPUIOWait float64
	MemoryPercent, Load1          float64
}

type hostDetails struct {
	Hostname, System, OS, Version, Release, Machine, Processor, PythonVersion, Model string
	Cores                                                                            int
	MemoryTotal, SwapUsed, SwapTotal                                                 uint64
	Load                                                                             [3]float64
	Uptime                                                                           time.Duration
	RLimitSoft, RLimitHard                                                           uint64
}

type interfaceDetails struct {
	Name, Address, Mask, Gateway string
}

type diskDetails struct {
	Mount, Device string
	Used, Total   uint64
	Percent       float64
}

func calculateRate(previous, current resourceSnapshot) resourceRate {
	seconds := current.TakenAt.Sub(previous.TakenAt).Seconds()
	if seconds <= 0 {
		seconds = 1
	}
	cpuDelta := delta(current.CPUTotal, previous.CPUTotal)
	percent := func(now, before uint64) float64 {
		if cpuDelta == 0 {
			return 0
		}
		return float64(delta(now, before)) / float64(cpuDelta) * 100
	}
	memoryPercent := 0.0
	if current.MemoryTotal > 0 {
		memoryPercent = float64(current.MemoryUsed) / float64(current.MemoryTotal) * 100
	}
	rate := resourceRate{
		NetIn:      float64(delta(current.NetInBytes, previous.NetInBytes)) / seconds,
		NetOut:     float64(delta(current.NetOutBytes, previous.NetOutBytes)) / seconds,
		PacketsIn:  float64(delta(current.PacketsIn, previous.PacketsIn)) / seconds,
		PacketsOut: float64(delta(current.PacketsOut, previous.PacketsOut)) / seconds,
		DiskRead:   float64(delta(current.DiskRead, previous.DiskRead)) / seconds,
		DiskWrite:  float64(delta(current.DiskWrite, previous.DiskWrite)) / seconds,
		CPUUser:    percent(current.CPUUser, previous.CPUUser), CPUSystem: percent(current.CPUSystem, previous.CPUSystem), CPUIOWait: percent(current.CPUIOWait, previous.CPUIOWait),
		MemoryPercent: memoryPercent, Load1: current.Load1,
	}
	if current.CPUInstant {
		rate.CPUUser = float64(current.CPUUser) / 100
		rate.CPUSystem = float64(current.CPUSystem) / 100
		rate.CPUIOWait = float64(current.CPUIOWait) / 100
	}
	return rate
}

func delta(current, previous uint64) uint64 {
	if current < previous {
		return 0
	}
	return current - previous
}

// formatRate는 5자를 넘지 않도록 값 크기에 따라 단위와 소수 자릿수를 줄인다.
func formatRate(bytes float64) string {
	value := bytes / (1024 * 1024)
	unit := "M"
	if value >= 1024 {
		value /= 1024
		unit = "G"
	}
	switch {
	case value >= 100:
		return fmt.Sprintf("%.0f%s", value, unit)
	case value >= 10:
		return fmt.Sprintf("%.1f%s", value, unit)
	default:
		return fmt.Sprintf("%.2f%s", value, unit)
	}
}
func formatBytes(bytes uint64) string {
	const (
		gib = 1024 * 1024 * 1024
		tib = 1024 * gib
	)
	if bytes >= tib {
		return fmt.Sprintf("%.2f TB", float64(bytes)/tib)
	}
	return fmt.Sprintf("%.2f GB", float64(bytes)/gib)
}

func networkInterfaces(defaultInterface, gateway string) ([]interfaceDetails, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var result []interfaceDetails
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			ipnet, ok := address.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			mask := net.IP(ipnet.Mask).String()
			item := interfaceDetails{Name: iface.Name, Address: ipnet.IP.String(), Mask: mask}
			if iface.Name == defaultInterface {
				item.Gateway = gateway
			}
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func runtimeMachine() string { return runtime.GOARCH }

func detectPythonVersion() string {
	for _, name := range []string{"python3", "python"} {
		output, err := exec.Command(name, "--version").CombinedOutput()
		if err == nil {
			return strings.TrimPrefix(strings.TrimSpace(string(output)), "Python ")
		}
	}
	return "not installed"
}

func collectDisksFromDF() ([]diskDetails, error) {
	output, err := exec.Command("df", "-kP").Output()
	if err != nil {
		return nil, err
	}
	var disks []diskDetails
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 || fields[0] == "Filesystem" {
			continue
		}
		if strings.HasPrefix(fields[0], "devfs") || strings.HasPrefix(fields[0], "map ") {
			continue
		}
		totalKB, err1 := strconv.ParseUint(fields[1], 10, 64)
		usedKB, err2 := strconv.ParseUint(fields[2], 10, 64)
		if err1 != nil || err2 != nil || totalKB == 0 {
			continue
		}
		disks = append(disks, diskDetails{Mount: strings.Join(fields[5:], " "), Device: fields[0], Total: totalKB * 1024, Used: usedKB * 1024, Percent: float64(usedKB) / float64(totalKB) * 100})
	}
	return disks, scanner.Err()
}
