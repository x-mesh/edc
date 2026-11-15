//go:build linux

package edc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"
)

// 고른 파일만 읽은 기록이 journal 전체를 읽은 기록과 같은지 이 호스트에서 비교한다. journal 전체를 읽어 느리므로 EDC_TEST_HOST_JOURNAL=1일 때만 돈다.
func TestHostAuditJournalFilesKeepEveryEntry(t *testing.T) {
	if os.Getenv("EDC_TEST_HOST_JOURNAL") != "1" {
		t.Skip("EDC_TEST_HOST_JOURNAL=1 is not set")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root to read the system journal")
	}
	now := time.Now()
	since := now.Add(-auditJournalWindow)
	files := auditJournalFiles(auditJournalDirs("/"), since)
	if files == nil {
		t.Fatal("no journal files selected")
	}
	t.Logf("selected %d files", len(files))
	// 읽는 동안 쓰인 기록이 비교에 섞이지 않게 끝 시각을 고정한다.
	window := []string{"--no-pager", "-o", "json", "--output-fields=SYSLOG_IDENTIFIER", "--since", fmt.Sprintf("@%d", since.Unix()), "--until", fmt.Sprintf("@%d", now.Unix()-1)}
	for _, identifiers := range [][]string{{"systemd-logind"}, auditSSHDIdentifiers} {
		var match []string
		for _, identifier := range identifiers {
			match = append(match, "-t", identifier)
		}
		all := testJournalCursors(t, slices.Concat(window, match))
		var selected []string
		for _, file := range files {
			selected = append(selected, "--file="+file)
		}
		chosen := testJournalCursors(t, slices.Concat(window, selected, match))
		if !slices.Equal(all, chosen) {
			t.Errorf("%v: all files %d entries, selected files %d entries", identifiers, len(all), len(chosen))
		}
		t.Logf("%v: %d entries", identifiers, len(all))
	}
}

// testJournalCursors는 기록의 __CURSOR를 정렬한 것이다. 여러 파일에 같은 시각의 기록이 있으면 출력 순서가 달라질 수 있다.
func testJournalCursors(t *testing.T, args []string) []string {
	t.Helper()
	output, err := exec.Command("journalctl", args...).Output()
	if err != nil {
		t.Fatalf("journalctl %q: %v", args, err)
	}
	var cursors []string
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 64<<10), eventsJournalLineLimit)
	for scanner.Scan() {
		var entry struct {
			Cursor string `json:"__CURSOR"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("journal line %q: %v", scanner.Text(), err)
		}
		cursors = append(cursors, entry.Cursor)
	}
	slices.Sort(cursors)
	return cursors
}
