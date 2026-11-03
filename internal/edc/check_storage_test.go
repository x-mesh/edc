package edc

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

const testMdstat = `Personalities : [raid1] [raid6] [raid5] [raid4]
md2 : active raid5 sdd1[3] sdc1[1] sdb1[0]
      2093056 blocks super 1.2 level 5, 512k chunk, algorithm 2 [3/3] [UUU]
      [====>................]  check = 22.5% (236032/1046528) finish=0.2min speed=59008K/sec

md1 : active raid1 sdb2[1](F) sda2[0]
      1047552 blocks super 1.2 [2/1] [U_]

md0 : active raid1 sdc2[2] sda1[0]
      1047552 blocks super 1.2 [2/1] [U_]
      [=>...................]  recovery = 7.3% (77440/1047552) finish=0.4min speed=38720K/sec

md127 : inactive sde[0](S)
      1047552 blocks super 1.2

unused devices: <none>
`

func TestParseMdstatReadsMissingMembersAndSync(t *testing.T) {
	arrays := parseMdstat(testMdstat)
	want := []checkArray{
		{Name: "md2", Level: "raid5"},
		{Name: "md1", Level: "raid1", Missing: 1},
		{Name: "md0", Level: "raid1", Missing: 1, Sync: "recovery 7.3%"},
		{Name: "md127", Inactive: true},
	}
	if fmt.Sprint(arrays) != fmt.Sprint(want) {
		t.Fatalf("arrays = %+v\nwant %+v", arrays, want)
	}
}

func TestParseSmartctlReportReadsNVMeAndATA(t *testing.T) {
	nvme := `{"smartctl":{"exit_status":0},"model_name":"Samsung SSD 980","smart_status":{"passed":true},
		"nvme_smart_health_information_log":{"critical_warning":0,"available_spare":8,"available_spare_threshold":10,"percentage_used":93,"media_errors":2}}`
	device, err := parseSmartctlReport("/dev/nvme0", []byte(nvme))
	if err != nil || device.Wear != 93 || !device.SpareLow || device.MediaErrors != 2 || device.Failing {
		t.Fatalf("nvme = %+v, %v", device, err)
	}
	// bit 3은 디스크가 실패한다는 판정이다. exit status가 0이 아니어도 값을 읽는다.
	ata := `{"smartctl":{"exit_status":8},"model_name":"WDC WD40","smart_status":{"passed":false},
		"ata_smart_attributes":{"table":[{"id":5,"raw":{"value":12}},{"id":9,"raw":{"value":40000}},{"id":197,"raw":{"value":3}},{"id":198,"raw":{"value":1}}]}}`
	device, err = parseSmartctlReport("/dev/sda", []byte(ata))
	if err != nil || !device.Failing || device.Wear != -1 || device.Reallocated != 12 || device.Pending != 3 || device.Uncorrectable != 1 {
		t.Fatalf("ata = %+v, %v", device, err)
	}
	denied := `{"smartctl":{"exit_status":2,"messages":[{"string":"Smartctl open device: /dev/sda failed: Permission denied"}]}}`
	if _, err := parseSmartctlReport("/dev/sda", []byte(denied)); err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("open failure = %v", err)
	}
}

func TestParseDiskutilSMARTReadsAppleNVMeKeys(t *testing.T) {
	info := map[string]any{
		"DeviceIdentifier": "disk0", "MediaName": "APPLE SSD AP1024Z", "SMARTStatus": "Verified",
		"SMARTDeviceSpecificKeysMayVaryNotGuaranteed": map[string]any{
			"PERCENTAGE_USED": int64(4), "AVAILABLE_SPARE": int64(100), "AVAILABLE_SPARE_THRESHOLD": int64(99), "MEDIA_ERRORS_0": int64(0),
		},
	}
	device, ok := parseDiskutilSMART(info)
	if !ok || device.Name != "disk0" || device.Wear != 4 || device.SpareLow || device.Failing || device.MediaErrors != 0 {
		t.Fatalf("device = %+v, %v", device, ok)
	}
	if _, ok := parseDiskutilSMART(map[string]any{"DeviceIdentifier": "disk4", "SMARTStatus": "Not Supported"}); ok {
		t.Fatal("a disk without SMART must be left out")
	}
	failing, _ := parseDiskutilSMART(map[string]any{"DeviceIdentifier": "disk2", "SMARTStatus": "Failing"})
	if !failing.Failing || failing.Wear != -1 {
		t.Fatalf("failing = %+v", failing)
	}
}

func TestCheckStorageFailsOnDegradedArrayAndPointsAtIt(t *testing.T) {
	storage := checkStorage{
		Arrays:     parseMdstat(testMdstat),
		Devices: []checkStorageDevice{{Name: "/dev/nvme0", Wear: 91, Next: "smartctl -a -d nvme /dev/nvme0"}},
	}
	result := checkStorageResult(checkFacts{Storage: storage})
	if result.Status != StatusFail || result.Next != "cat /proc/mdstat" {
		t.Fatalf("result = %+v", result)
	}
	for _, text := range []string{"md1 (raid1) is degraded, 1 members missing", "md0", "md127 is inactive", "/dev/nvme0: 91%"} {
		if !strings.Contains(result.Summary, text) {
			t.Errorf("summary misses %q: %s", text, result.Summary)
		}
	}
	// check는 정기 scrub이라 진행 중으로 보지 않는다.
	if strings.Contains(result.Summary, "md2") {
		t.Errorf("a scrub must not warn: %s", result.Summary)
	}
}

func TestCheckStorageNextPointsAtTheWorstDevice(t *testing.T) {
	storage := checkStorage{Devices: []checkStorageDevice{
		{Name: "/dev/bus/0 [megaraid,0]", Wear: -1, Pending: 1, Next: "smartctl -a -d megaraid,0 /dev/bus/0"},
		{Name: "/dev/bus/0 [megaraid,1]", Wear: -1, Failing: true, Next: "smartctl -a -d megaraid,1 /dev/bus/0"},
	}}
	result := checkStorageResult(checkFacts{Storage: storage})
	if result.Status != StatusFail || result.Next != "smartctl -a -d megaraid,1 /dev/bus/0" || !strings.Contains(result.Summary, "[megaraid,1]: SMART") {
		t.Fatalf("result = %+v", result)
	}
}

func TestCheckStoragePassesHealthyDevices(t *testing.T) {
	storage := checkStorage{Devices: []checkStorageDevice{{Name: "disk0", Wear: 4, Next: "diskutil info disk0"}}}
	result := checkStorageResult(checkFacts{Storage: storage})
	if result.Status != StatusPass || result.Summary != T("check.storage.ok", 1, 0) || len(result.Warnings) != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestCheckStorageSkipsWithoutASourceAndSaysWhy(t *testing.T) {
	cases := map[string]struct {
		storage checkStorage
		want    string
	}{
		"no smartctl": {checkStorage{DevicesErr: errCheckNoSmartctl}, T("check.storage.no_smartctl")},
		"not root":    {checkStorage{DevicesErr: fmt.Errorf("smartctl: %w", fs.ErrPermission)}, T("check.skip.needs_root", "SMART")},
		"nothing":     {checkStorage{}, T("check.storage.none")},
	}
	for name, test := range cases {
		result := checkStorageResult(checkFacts{Storage: test.storage})
		if result.Status != StatusSkip || result.Summary != test.want {
			t.Errorf("%s: result = %+v", name, result)
		}
	}
}

func TestCheckStorageJudgesArraysWhenSMARTIsUnread(t *testing.T) {
	storage := checkStorage{Arrays: []checkArray{{Name: "md0", Level: "raid1"}}, DevicesErr: errors.New("smartctl: permission denied")}
	result := checkStorageResult(checkFacts{Storage: storage})
	if result.Status != StatusPass || len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "permission denied") {
		t.Fatalf("result = %+v", result)
	}
}
