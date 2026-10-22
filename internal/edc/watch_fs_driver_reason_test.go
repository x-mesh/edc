package edc

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func fsWatchReadyLine(t *testing.T, record fsWatchRecord, jsonOutput bool) string {
	t.Helper()
	var output bytes.Buffer
	if err := writeFSWatchRecord(&output, record, jsonOutput, textStyle{}); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(output.String())
}

// auto가 inotify를 고른 이유는 시작 줄에 붙는다. 이유가 없으면(직접 고른 방식, fanotify) 이전 줄과 같다.
func TestFSWatchReadyLineNamesWhyAutoChoseInotify(t *testing.T) {
	previous := currentLanguage()
	defer setLanguage(previous)
	base := fsWatchRecord{Type: "ready", Time: time.Date(2026, 10, 22, 7, 0, 0, 0, time.UTC), Root: "/app"}
	for _, test := range []struct {
		language, driver, reason, want string
		recursive                      bool
	}{
		{"en", "inotify", fsWatchReasonNotRecursive, `WATCH "/app" · recursive=false · driver=inotify (not recursive)`, false},
		{"en", "inotify", fsWatchReasonFanotifyDown, `WATCH "/app" · recursive=true · driver=inotify (fanotify unavailable)`, true},
		{"en", "inotify", "", `WATCH "/app" · recursive=true · driver=inotify`, true},
		{"en", "fanotify", "", `WATCH "/app" · recursive=true · driver=fanotify`, true},
		{"ko", "inotify", fsWatchReasonNotRecursive, `WATCH "/app" · recursive=false · driver=inotify (재귀 감시 아님)`, false},
		{"ja", "inotify", fsWatchReasonFanotifyDown, `WATCH "/app" · recursive=true · driver=inotify (fanotify 使用不可)`, true},
	} {
		setLanguage(test.language)
		record := base
		record.Driver, record.DriverReason, record.Recursive = test.driver, test.reason, test.recursive
		line := fsWatchReadyLine(t, record, false)
		if !strings.HasSuffix(line, test.want) {
			t.Errorf("%s %s/%s: line = %q, want it to end with %q", test.language, test.driver, test.reason, line, test.want)
		}
	}
}

// JSON 시작 기록은 이유를 코드로 낸다. 이유가 없으면 필드를 내지 않아 이전 출력과 같다.
func TestFSWatchReadyRecordCarriesTheDriverReasonInJSON(t *testing.T) {
	record := fsWatchRecord{Type: "ready", Time: time.Date(2026, 10, 22, 7, 0, 0, 0, time.UTC), Root: "/app", Driver: "inotify", DriverReason: fsWatchReasonNotRecursive}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(fsWatchReadyLine(t, record, true)), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["driver_reason"] != "not_recursive" || decoded["driver"] != "inotify" {
		t.Errorf("record = %v", decoded)
	}
	record.DriverReason = ""
	if line := fsWatchReadyLine(t, record, true); strings.Contains(line, "driver_reason") {
		t.Errorf("an empty reason must not appear: %s", line)
	}
}
