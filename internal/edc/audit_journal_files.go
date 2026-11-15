package edc

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// journal 파일 header에서 읽는 위치다. systemd v249부터 main까지 같다(src/libsystemd/sd-journal/journal-def.h의 struct Header).
const (
	auditJournalHeaderSize         = 200
	auditJournalHeaderState        = 16
	auditJournalHeaderEntries      = 152
	auditJournalHeaderTailRealtime = 192
	auditJournalStateArchived      = 2
	auditJournalSignature          = "LPKSHHRH"
)

// auditJournalDirs는 인자 없는 journalctl이 읽는 디렉터리다. systemd의 add_search_paths와 add_directory를 따른다.
// 두 root 바로 아래 파일, /var 쪽은 이 호스트의 machine-id 디렉터리, /run 쪽은 namespace가 없는 모든 machine-id 디렉터리다.
// 읽지 못하면 nil이다.
func auditJournalDirs(root string) []string {
	data, err := os.ReadFile(filepath.Join(root, "etc", "machine-id"))
	if err != nil {
		return nil
	}
	machineID := strings.TrimSpace(string(data))
	if !auditJournalIsID(machineID) {
		return nil
	}
	persistent, runtime := filepath.Join(root, "var", "log", "journal"), filepath.Join(root, "run", "log", "journal")
	dirs := []string{persistent, filepath.Join(persistent, machineID), runtime}
	entries, err := os.ReadDir(runtime)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() || !auditJournalIsID(entry.Name()) {
			continue
		}
		dirs = append(dirs, filepath.Join(runtime, entry.Name()))
	}
	return dirs
}

func auditJournalIsID(name string) bool {
	_, err := hex.DecodeString(name)
	return len(name) == 32 && err == nil
}

// auditJournalFiles는 dirs의 journal 파일 중 since 뒤의 기록이 있을 수 있는 것이다.
// 보관이 끝났고 마지막 기록이 since 앞이라고 header로 확인한 파일만 뺀다. 기간 밖 파일을 열면 cache가 빈 호스트에서 수백 MB를 읽기 때문이다.
// 디렉터리를 읽지 못하거나 고른 파일이 없으면 nil이라, 부르는 쪽이 journalctl의 기본 위치를 모두 읽는다.
func auditJournalFiles(dirs []string, since time.Time) []string {
	var files []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.Type().IsRegular() && entry.Type()&fs.ModeSymlink == 0 {
				continue
			}
			if !strings.HasSuffix(name, ".journal") && !strings.HasSuffix(name, ".journal~") {
				continue
			}
			path := filepath.Join(dir, name)
			if auditJournalFileBefore(path, since) {
				continue
			}
			files = append(files, path)
		}
	}
	return files
}

func auditJournalFileBefore(path string, since time.Time) bool {
	if strings.HasSuffix(path, "~") {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	header := make([]byte, auditJournalHeaderSize)
	if _, err := io.ReadFull(file, header); err != nil {
		return false
	}
	return auditJournalArchivedBefore(header, since)
}

// auditJournalArchivedBefore는 header가 보관이 끝난 파일이고 그 마지막 기록이 since 앞인지다.
// online이나 offline 파일은 아직 쓰이거나 비정상 종료로 남은 것이라 header의 시각을 믿지 않는다. 판정할 수 없으면 false다.
func auditJournalArchivedBefore(header []byte, since time.Time) bool {
	if len(header) < auditJournalHeaderSize || string(header[:len(auditJournalSignature)]) != auditJournalSignature ||
		header[auditJournalHeaderState] != auditJournalStateArchived ||
		binary.LittleEndian.Uint64(header[auditJournalHeaderEntries:]) == 0 {
		return false
	}
	tail := binary.LittleEndian.Uint64(header[auditJournalHeaderTailRealtime:])
	limit := since.UnixMicro()
	return tail != 0 && limit > 0 && tail < uint64(limit)
}
