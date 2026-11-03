package edc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// check.storage는 디스크 장치와 md RAID의 건강이다. 사용률은 check.disk가 보므로 여기서는 장치가 망가지는 신호만 본다.

// checkWearLimits는 NVMe percentage_used의 기준이다. 100은 제조사가 보증한 쓰기 수명을 다 쓴 것이다.
var checkWearLimits = topThreshold{warn: 90, danger: 100}

// checkStorageDevice는 SMART를 읽은 장치 하나다. Wear가 -1이면 수명 값을 주지 않는 장치다.
type checkStorageDevice struct {
	// Next는 이 장치를 더 볼 명령이다. MegaRAID 뒤의 디스크는 경로가 같아 -d 값까지 있어야 한 장치를 가리킨다.
	Name, Model, Next string
	Failing     bool
	Wear        int
	// SpareLow는 남은 예비 블록이 제조사의 기준보다 적다는 것이다.
	SpareLow    bool
	MediaErrors uint64
	// Reallocated, Pending, Uncorrectable은 ATA 속성 5, 197, 198의 raw 값이다. 0이 아니면 표면에 문제가 생긴 것이다.
	Reallocated, Pending, Uncorrectable uint64
}

// checkArray는 md RAID 하나다. Missing은 빠진 구성원 수이고 Sync는 진행 중인 recovery·resync·reshape다.
type checkArray struct {
	Name, Level string
	Inactive    bool
	Missing     int
	Sync        string
}

type checkStorage struct {
	Devices []checkStorageDevice
	Arrays  []checkArray
	// DevicesErr는 SMART를, ArraysErr는 /proc/mdstat을 읽지 못한 이유다. 한쪽만 읽었으면 그쪽은 판정한다.
	DevicesErr, ArraysErr error
}

// errCheckNoSmartctl은 smartctl이 없는 Linux다. 장치 건강은 skip이고 설치 방법을 알린다.
var errCheckNoSmartctl = &messageError{key: "check.storage.no_smartctl"}

func checkStorageResult(facts checkFacts) Result {
	const id = "storage"
	storage := facts.Storage
	if len(storage.Devices) == 0 && len(storage.Arrays) == 0 {
		var result Result
		switch {
		case errors.Is(storage.DevicesErr, errCheckNoSmartctl):
			result = checkResult(id, StatusSkip, storage.DevicesErr.Error())
		case storage.DevicesErr != nil:
			result = checkSkip(id, "SMART", storage.DevicesErr)
		case storage.ArraysErr != nil:
			return checkSkip(id, "/proc/mdstat", storage.ArraysErr)
		default:
			return checkResult(id, StatusSkip, T("check.storage.none"))
		}
		if storage.ArraysErr != nil {
			result.Warnings = append(result.Warnings, T("check.storage.arrays_unread", infoSingleLine(storage.ArraysErr.Error())))
		}
		return result
	}
	var issues checkIssues
	next := ""
	// next는 가장 나쁜 판정의 첫 장치를 가리킨다.
	flag := func(status Status, line, command string) {
		if next == "" || checkSeverity(status) > checkSeverity(issues.status) {
			next = command
		}
		issues.add(status, line)
	}
	for _, array := range storage.Arrays {
		command := "cat /proc/mdstat"
		switch {
		case array.Inactive:
			flag(StatusFail, T("check.storage.array_inactive", array.Name), command)
		case array.Missing > 0:
			flag(StatusFail, T("check.storage.array_degraded", array.Name, array.Level, array.Missing), command)
		case array.Sync != "":
			flag(StatusWarn, T("check.storage.array_sync", array.Name, array.Sync), command)
		}
	}
	for _, device := range storage.Devices {
		command := device.Next
		if device.Failing {
			flag(StatusFail, T("check.storage.failing", device.Name), command)
		}
		if device.SpareLow {
			flag(StatusFail, T("check.storage.spare_low", device.Name), command)
		}
		if status := checkUsageStatus(float64(device.Wear), checkWearLimits); device.Wear >= 0 && status != StatusPass {
			flag(status, T("check.storage.wear", device.Name, device.Wear), command)
		}
		if device.MediaErrors > 0 {
			flag(StatusWarn, T("check.storage.media_errors", device.Name, device.MediaErrors), command)
		}
		if bad := device.Reallocated + device.Pending + device.Uncorrectable; bad > 0 {
			flag(StatusWarn, T("check.storage.sectors", device.Name, device.Reallocated, device.Pending, device.Uncorrectable), command)
		}
	}
	result := issues.result(id, T("check.storage.ok", len(storage.Devices), len(storage.Arrays)), next)
	// RAID만 읽고 SMART를 못 읽었으면 장치 쪽은 판정하지 않았다는 것을 남긴다. pass로 읽히면 안 된다.
	if storage.DevicesErr != nil {
		result.Warnings = append(result.Warnings, T("check.storage.devices_unread", infoSingleLine(storage.DevicesErr.Error())))
	}
	if storage.ArraysErr != nil {
		result.Warnings = append(result.Warnings, T("check.storage.arrays_unread", infoSingleLine(storage.ArraysErr.Error())))
	}
	return result
}

var (
	mdstatArray  = regexp.MustCompile(`^(md\S*) : (active|inactive)(?: \((?:auto-)?read-only\))?(?: (\S+))?`)
	mdstatStatus = regexp.MustCompile(`\[(\d+)/(\d+)\]`)
	mdstatSync   = regexp.MustCompile(`(recovery|resync|reshape)\s*=\s*([\d.]+%)`)
)

// parseMdstat은 /proc/mdstat의 배열을 읽는다. check(scrub)는 주기적으로 도는 정상 작업이라 진행 중으로 보지 않는다.
func parseMdstat(text string) []checkArray {
	var arrays []checkArray
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := scanner.Text()
		if match := mdstatArray.FindStringSubmatch(line); match != nil {
			array := checkArray{Name: match[1], Inactive: match[2] == "inactive", Level: match[3]}
			// inactive 배열은 level 자리에 구성원 장치가 온다.
			if array.Inactive || strings.Contains(array.Level, "[") {
				array.Level = ""
			}
			arrays = append(arrays, array)
			continue
		}
		if len(arrays) == 0 || !strings.HasPrefix(line, " ") {
			continue
		}
		current := &arrays[len(arrays)-1]
		if match := mdstatStatus.FindStringSubmatch(line); match != nil {
			want, _ := strconv.Atoi(match[1])
			have, _ := strconv.Atoi(match[2])
			current.Missing = max(want-have, 0)
		}
		if match := mdstatSync.FindStringSubmatch(line); match != nil {
			current.Sync = match[1] + " " + match[2]
		}
	}
	return arrays
}

// smartctlScan은 smartctl --scan --json의 장치 목록이다.
type smartctlScan struct {
	Devices []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"devices"`
}

type smartctlReport struct {
	Smartctl struct {
		ExitStatus int `json:"exit_status"`
		Messages   []struct {
			String string `json:"string"`
		} `json:"messages"`
	} `json:"smartctl"`
	ModelName   string `json:"model_name"`
	SmartStatus *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	NVMe *struct {
		CriticalWarning int    `json:"critical_warning"`
		AvailableSpare  int    `json:"available_spare"`
		SpareThreshold  int    `json:"available_spare_threshold"`
		PercentageUsed  int    `json:"percentage_used"`
		MediaErrors     uint64 `json:"media_errors"`
	} `json:"nvme_smart_health_information_log"`
	ATA *struct {
		Table []struct {
			ID  int `json:"id"`
			Raw struct {
				Value uint64 `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
}

// smartctlOpenFailed는 smartctl exit status의 bit 1이다. 장치를 열지 못했다는 뜻이고, root가 아닐 때 흔하다.
// smartctlCommandFailed는 bit 2로, SMART 명령이 실패했거나 SMART 자료의 checksum이 맞지 않는다는 뜻이다.
const (
	smartctlOpenFailed    = 1 << 1
	smartctlCommandFailed = 1 << 2
)

// parseSmartctlReport는 smartctl --json -H -A의 출력이다. exit status는 bit mask라 0이 아니어도 판정 값은 읽는다.
func parseSmartctlReport(name string, data []byte) (checkStorageDevice, error) {
	var report smartctlReport
	if err := json.Unmarshal(data, &report); err != nil {
		return checkStorageDevice{}, fmt.Errorf("smartctl %s: %w", name, err)
	}
	message := func(fallback string) error {
		if messages := report.Smartctl.Messages; len(messages) > 0 {
			fallback = messages[0].String
		}
		return fmt.Errorf("smartctl %s: %s", name, fallback)
	}
	if report.Smartctl.ExitStatus&smartctlOpenFailed != 0 {
		return checkStorageDevice{}, message("open failed")
	}
	// 판정 값이 하나도 없으면 정상으로 셀 근거가 없다. bit 2가 함께 와도 판정 값이 있으면 일부 log만 읽지 못한 것이다.
	if report.SmartStatus == nil && report.NVMe == nil && report.ATA == nil {
		if report.Smartctl.ExitStatus&smartctlCommandFailed != 0 {
			return checkStorageDevice{}, message("SMART command failed")
		}
		return checkStorageDevice{}, message("no SMART health data")
	}
	device := checkStorageDevice{Name: name, Model: report.ModelName, Wear: -1}
	if report.SmartStatus != nil {
		device.Failing = !report.SmartStatus.Passed
	}
	if nvme := report.NVMe; nvme != nil {
		device.Wear, device.MediaErrors = nvme.PercentageUsed, nvme.MediaErrors
		device.SpareLow = nvme.SpareThreshold > 0 && nvme.AvailableSpare < nvme.SpareThreshold
	}
	if ata := report.ATA; ata != nil {
		for _, attribute := range ata.Table {
			switch attribute.ID {
			case 5:
				device.Reallocated = attribute.Raw.Value
			case 197:
				device.Pending = attribute.Raw.Value
			case 198:
				device.Uncorrectable = attribute.Raw.Value
			}
		}
	}
	return device, nil
}

// parseDiskutilSMART는 diskutil info -plist 한 장치다. SMARTStatus가 Verified나 Failing이 아니면 SMART를 주지 않는 장치라 뺀다.
// NVMe 값은 diskutil이 "보장하지 않는다"고 이름 붙인 dict에 있어, 없으면 수명은 모르는 값으로 둔다.
func parseDiskutilSMART(info map[string]any) (checkStorageDevice, bool) {
	status, _ := info["SMARTStatus"].(string)
	if status != "Verified" && status != "Failing" {
		return checkStorageDevice{}, false
	}
	device := checkStorageDevice{Wear: -1, Failing: status == "Failing"}
	device.Name, _ = info["DeviceIdentifier"].(string)
	device.Model, _ = info["MediaName"].(string)
	keys, _ := info["SMARTDeviceSpecificKeysMayVaryNotGuaranteed"].(map[string]any)
	if used, ok := keys["PERCENTAGE_USED"].(int64); ok {
		device.Wear = int(used)
	}
	if errors, ok := keys["MEDIA_ERRORS_0"].(int64); ok && errors > 0 {
		device.MediaErrors = uint64(errors)
	}
	spare, spareOK := keys["AVAILABLE_SPARE"].(int64)
	threshold, thresholdOK := keys["AVAILABLE_SPARE_THRESHOLD"].(int64)
	device.SpareLow = spareOK && thresholdOK && threshold > 0 && spare < threshold
	return device, true
}
