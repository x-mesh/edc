package edc

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func testJournalHeader(state byte, entries uint64, tail time.Time) []byte {
	header := make([]byte, 256)
	copy(header, auditJournalSignature)
	header[auditJournalHeaderState] = state
	binary.LittleEndian.PutUint64(header[auditJournalHeaderEntries:], entries)
	if !tail.IsZero() {
		binary.LittleEndian.PutUint64(header[auditJournalHeaderTailRealtime:], uint64(tail.UnixMicro()))
	}
	return header
}

// 보관이 끝났고 마지막 기록이 since 앞이라고 확인한 파일만 뺀다. 판정할 수 없는 파일은 모두 읽는다.
func TestAuditJournalArchivedBefore(t *testing.T) {
	since := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	old, recent := since.Add(-time.Hour), since.Add(time.Hour)
	wrongSignature := testJournalHeader(auditJournalStateArchived, 10, old)
	copy(wrongSignature, "XXXXXXXX")
	for _, test := range []struct {
		name   string
		header []byte
		want   bool
	}{
		{"archived old", testJournalHeader(auditJournalStateArchived, 10, old), true},
		{"archived at since", testJournalHeader(auditJournalStateArchived, 10, since), false},
		{"archived recent", testJournalHeader(auditJournalStateArchived, 10, recent), false},
		{"online old", testJournalHeader(1, 10, old), false},
		{"offline old", testJournalHeader(0, 10, old), false},
		{"no entries", testJournalHeader(auditJournalStateArchived, 0, old), false},
		{"no tail time", testJournalHeader(auditJournalStateArchived, 10, time.Time{}), false},
		{"wrong signature", wrongSignature, false},
		{"short", testJournalHeader(auditJournalStateArchived, 10, old)[:auditJournalHeaderSize-1], false},
	} {
		if got := auditJournalArchivedBefore(test.header, since); got != test.want {
			t.Errorf("%s = %v, want %v", test.name, got, test.want)
		}
	}
}

// 인자 없는 journalctl이 읽는 위치와 같다. /var 쪽은 이 호스트의 machine-id만, /run 쪽은 namespace가 없는 machine-id 디렉터리를 모두 본다.
func TestAuditJournalDirs(t *testing.T) {
	root := t.TempDir()
	machineID := "8853ade3b0014f8b87423f161362c3b3"
	other := "6e13591524a9443da2e45c802793c731"
	for _, dir := range []string{"etc", "var/log/journal/" + machineID, "var/log/journal/" + other, "run/log/journal/" + other, "run/log/journal/" + machineID + ".ns", "run/log/journal/notes"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "etc/machine-id"), []byte(machineID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(root, "var/log/journal"), filepath.Join(root, "var/log/journal", machineID),
		filepath.Join(root, "run/log/journal"), filepath.Join(root, "run/log/journal", other),
	}
	if got := auditJournalDirs(root); !slices.Equal(got, want) {
		t.Fatalf("dirs = %q, want %q", got, want)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/machine-id"), []byte("uninitialized\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := auditJournalDirs(root); got != nil {
		t.Fatalf("dirs with a bad machine-id = %q, want nil", got)
	}
}

func TestAuditJournalFiles(t *testing.T) {
	since := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	dir, runtime := t.TempDir(), t.TempDir()
	old := testJournalHeader(auditJournalStateArchived, 10, since.Add(-time.Hour))
	files := map[string][]byte{
		"system@a-1.journal":          old,
		"user-1000@a-1.journal":       old,
		"system@a-2.journal":          testJournalHeader(auditJournalStateArchived, 10, since.Add(time.Hour)),
		"system.journal":              testJournalHeader(1, 10, since.Add(-time.Hour)),
		"system@b-1.journal~":         old,
		"broken.journal":              []byte("short"),
		"system@a-1.journal.download": old,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub.journal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtime, "system.journal"), testJournalHeader(1, 10, since), 0o640); err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(dir, "broken.journal"), filepath.Join(dir, "system.journal"), filepath.Join(dir, "system@a-2.journal"),
		filepath.Join(dir, "system@b-1.journal~"), filepath.Join(runtime, "system.journal"),
	}
	got := auditJournalFiles([]string{dir, filepath.Join(dir, "missing"), runtime}, since)
	if !slices.Equal(got, want) {
		t.Fatalf("files = %q, want %q", got, want)
	}
	if got := auditJournalFiles([]string{filepath.Join(dir, "missing")}, since); got != nil {
		t.Fatalf("files without a journal = %q, want nil", got)
	}
}
