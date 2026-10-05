package edc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	historyFamilyLimit  = 1000
	historyByteLimit    = 128 * 1024 * 1024
	historyAttemptLimit = 10000
	historyMarkerLimit  = 64 * 1024
)

type logHistoryAttempt struct {
	Key, CWD, Path, Display, Outcome     string
	Command                              []string
	Started, Ended                       time.Time
	Duration                             time.Duration
	Exit                                 *int
	Attempt, Ordinal                     int
	damaged, processSeen, startErrorSeen bool
}

func (row logHistoryAttempt) failed() bool {
	return row.Outcome != "SUCCESS" && row.Outcome != "UNKNOWN"
}

type logHistorySnapshot struct {
	Rows   []logHistoryAttempt
	Issues []string
}

func (snapshot *logHistorySnapshot) issue(path, kind string) {
	snapshot.Issues = append(snapshot.Issues, T("cli.log_history."+kind, reportIdentityValue(path)))
}

type historyParser struct {
	rows        []logHistoryAttempt
	current     *logHistoryAttempt
	path        string
	invalid     bool
	ordinal     int
	nextAttempt int
}

func parseHistoryMarker(line string) (string, map[string]string, error) {
	const prefix = "=== edc log "
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, " ===") {
		return "", nil, errors.New("invalid marker")
	}
	text := strings.TrimSuffix(strings.TrimPrefix(line, prefix), " ===")
	kind, text, ok := strings.Cut(text, " ")
	if !ok {
		return "", nil, errors.New("missing fields")
	}
	fields := map[string]string{}
	for text != "" {
		text = strings.TrimLeft(text, " ")
		if text == "" {
			break
		}
		key, rest, found := strings.Cut(text, "=")
		if !found || key == "" || strings.ContainsAny(key, " \t\r\n") {
			return "", nil, errors.New("invalid field")
		}
		if _, exists := fields[key]; exists {
			return "", nil, errors.New("duplicate field")
		}
		if rest == "" {
			return "", nil, errors.New("missing value")
		}
		length := strings.IndexByte(rest, ' ')
		if rest[0] == '[' || rest[0] == '"' {
			decoder := json.NewDecoder(strings.NewReader(rest))
			var raw json.RawMessage
			if err := decoder.Decode(&raw); err != nil {
				return "", nil, err
			}
			length = int(decoder.InputOffset())
			if length < len(rest) && rest[length] != ' ' {
				return "", nil, errors.New("invalid JSON boundary")
			}
		} else if length < 0 {
			length = len(rest)
		}
		fields[key] = rest[:length]
		text = rest[length:]
	}
	return kind, fields, nil
}

func (parser *historyParser) finish() {
	if parser.current == nil {
		return
	}
	row := *parser.current
	if row.Outcome == "" || row.damaged {
		row.Outcome = "UNKNOWN"
	}
	parser.rows = append(parser.rows, row)
	parser.current = nil
}

func (parser *historyParser) damage() {
	parser.invalid = true
	if parser.current != nil {
		parser.current.damaged = true
	}
}

func validHistoryKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for _, ch := range key {
		if !strings.ContainsRune("0123456789abcdef", ch) {
			return false
		}
	}
	return true
}

func (parser *historyParser) line(line string) {
	if !strings.HasPrefix(line, "=== edc log ") {
		return
	}
	kind, fields, err := parseHistoryMarker(line)
	if err != nil {
		parser.damage()
		return
	}
	switch kind {
	case "start":
		parser.finish()
		parser.ordinal++
		row := &logHistoryAttempt{Path: parser.path, Ordinal: parser.ordinal, Attempt: max(1, parser.nextAttempt), Display: fields["command_display"]}
		parser.current = row
		parser.nextAttempt = 1
		row.Started, err = time.Parse(time.RFC3339Nano, fields["time"])
		if err != nil || row.Started.IsZero() {
			parser.damage()
		}
		if err := json.Unmarshal([]byte(fields["cwd"]), &row.CWD); err != nil {
			parser.damage()
		}
		if raw, ok := fields["command"]; ok {
			if err := json.Unmarshal([]byte(raw), &row.Command); err != nil || len(row.Command) == 0 {
				parser.damage()
			}
		}
		pid, pidErr := strconv.Atoi(fields["pid"])
		if pidErr != nil || pid < 1 {
			parser.damage()
		}
		switch fields["stream"] {
		case "stdout", "stderr", "both":
		default:
			parser.damage()
		}
		stored := fields["command_key"]
		switch row.Display {
		case "none":
			if stored != "" || len(row.Command) != 0 {
				parser.damage()
			}
		case "name":
			if len(row.Command) != 1 {
				parser.damage()
			}
			if stored != "" && validHistoryKey(stored) && fields["command_key_version"] == "1" {
				row.Key = stored
			} else if stored != "" {
				parser.damage()
			}
		case "full":
			row.Key, err = commandKey(row.Command)
			if err != nil {
				parser.damage()
			}
			if stored != row.Key || fields["command_key_version"] != "1" {
				row.Key = ""
				parser.damage()
			}
		case "":
			if len(row.Command) > 1 || len(row.Command) == 1 && strings.ContainsRune(row.Command[0], filepath.Separator) {
				row.Key, err = commandKey(row.Command)
				if err != nil {
					parser.damage()
				}
			} else {
				row.Display = "unknown"
			}
			if stored != "" {
				row.Key = ""
				parser.damage()
			}
		default:
			parser.damage()
		}
	case "process":
		if parser.current == nil {
			parser.damage()
			return
		}
		row := parser.current
		if row.processSeen {
			parser.damage()
		}
		row.processSeen = true
		pid, pidErr := strconv.Atoi(fields["pid"])
		var executable string
		if pidErr != nil || pid < 1 || json.Unmarshal([]byte(fields["executable"]), &executable) != nil || executable == "" || row.startErrorSeen {
			parser.damage()
		}
		row.Attempt, err = strconv.Atoi(fields["attempt"])
		if err != nil || row.Attempt < 1 {
			parser.damage()
		}
	case "start_error":
		if parser.current == nil {
			parser.damage()
			return
		}
		if parser.current.processSeen || parser.current.startErrorSeen {
			parser.damage()
		}
		parser.current.startErrorSeen = true
		var cause string
		if json.Unmarshal([]byte(fields["cause"]), &cause) != nil {
			parser.damage()
		}
	case "end":
		if parser.current == nil {
			if len(parser.rows) > 0 {
				parser.rows[len(parser.rows)-1].damaged = true
				parser.rows[len(parser.rows)-1].Outcome = "UNKNOWN"
			}
			parser.damage()
			return
		}
		row := parser.current
		row.Ended, err = time.Parse(time.RFC3339Nano, fields["time"])
		if err != nil || row.Ended.Before(row.Started) {
			parser.damage()
		}
		row.Duration, err = time.ParseDuration(fields["duration"])
		if err != nil || row.Duration < 0 {
			parser.damage()
		}
		exit, err := strconv.Atoi(fields["exit"])
		if err != nil || exit < 0 || exit > 255 {
			parser.damage()
		} else {
			row.Exit = &exit
		}
		if row.startErrorSeen && fields["status"] != "start_error" || row.Display != "" && row.Display != "unknown" && !row.processSeen && fields["status"] != "start_error" {
			parser.damage()
		}
		switch fields["status"] {
		case "exit":
			row.Outcome = "SUCCESS"
			if exit != 0 {
				row.Outcome = "FAIL"
			}
		case "timeout":
			row.Outcome = "TIMEOUT"
			if exit != 124 {
				parser.damage()
			}
		case "signal":
			row.Outcome = "SIGNAL"
			if exit < 129 || fields["signal"] == "" {
				parser.damage()
			}
		case "start_error", "log_error", "wait_error", "signal_error":
			row.Outcome = "ERROR"
			if exit != 2 {
				parser.damage()
			}
		default:
			parser.damage()
		}
		parser.finish()
	case "restart":
		if parser.current != nil {
			parser.damage()
		}
		parser.nextAttempt, err = strconv.Atoi(fields["next_attempt"])
		if err != nil || parser.nextAttempt < 2 {
			parser.damage()
		}
	case "stopped":
		if parser.current != nil {
			parser.damage()
		}
		parser.nextAttempt = 1
	default:
		parser.damage()
	}
}

func readHistoryLines(reader io.Reader, parser *historyParser) error {
	input := bufio.NewReaderSize(reader, historyMarkerLimit)
	var line []byte
	discard := false
	for {
		chunk, err := input.ReadSlice('\n')
		if !discard {
			if len(line)+len(chunk) > historyMarkerLimit {
				if strings.HasPrefix(string(line)+string(chunk[:min(len(chunk), 16)]), "=== edc log ") {
					parser.damage()
				}
				line = nil
				discard = true
			} else {
				line = append(line, chunk...)
			}
		}
		if err != bufio.ErrBufferFull {
			if !discard && len(line) > 0 {
				parser.line(strings.TrimRight(string(line), "\r\n"))
			}
			line = nil
			discard = false
			if len(parser.rows) > historyAttemptLimit {
				return errHistoryScanLimit
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil && err != bufio.ErrBufferFull {
			return err
		}
	}
}

type historyPart struct {
	path  string
	info  os.FileInfo
	index int
}

func historyFamily(path string) ([]historyPart, error) {
	var parts []historyPart
	base := filepath.Base(path)
	examined := 0
	err := walkHistoryDirectory(filepath.Dir(path), func(entry os.DirEntry) error {
		examined++
		if examined > 10000 {
			return errHistoryScanLimit
		}
		index := 0
		if entry.Name() != base {
			prefix := base + ".edc."
			if !strings.HasPrefix(entry.Name(), prefix) {
				return nil
			}
			suffix := strings.TrimPrefix(entry.Name(), prefix)
			var err error
			index, err = strconv.Atoi(suffix)
			if err != nil || index < 1 || strconv.Itoa(index) != suffix {
				return nil
			}
		}
		if len(parts) > logMaxKeepFiles {
			return errHistoryScanLimit
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink log")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("nonregular log")
		}
		parts = append(parts, historyPart{path: filepath.Join(filepath.Dir(path), entry.Name()), info: info, index: index})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].index > parts[j].index })
	if len(parts) == 0 {
		return nil, os.ErrNotExist
	}
	return parts, nil
}

func sameHistoryFamily(before, after []historyPart) bool {
	if len(before) != len(after) {
		return false
	}
	for i, part := range before {
		other := after[i]
		if part.path != other.path || !os.SameFile(part.info, other.info) || part.info.Size() != other.info.Size() || !part.info.ModTime().Equal(other.info.ModTime()) {
			return false
		}
	}
	return true
}

func readHistoryFamily(path string, parts []historyPart) ([]logHistoryAttempt, bool, error) {
	var files []*os.File
	defer func() {
		for _, file := range files {
			file.Close()
		}
	}()
	readers := make([]io.Reader, 0, len(parts))
	for _, part := range parts {
		file, err := os.OpenFile(part.path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil, false, err
		}
		files = append(files, file)
		info, err := file.Stat()
		if err != nil {
			return nil, false, err
		}
		if !info.Mode().IsRegular() || !os.SameFile(info, part.info) {
			return nil, false, errors.New("changed file")
		}
		readers = append(readers, io.LimitReader(file, part.info.Size()))
	}
	parser := &historyParser{path: path}
	err := readHistoryLines(io.MultiReader(readers...), parser)
	parser.finish()
	if err != nil {
		return nil, false, err
	}
	after, err := historyFamily(path)
	if err != nil || !sameHistoryFamily(parts, after) {
		return nil, false, errors.New("changed file")
	}
	return parser.rows, parser.invalid, nil
}

var errHistoryScanLimit = errors.New("history scan limit")

func walkHistoryDirectory(directory string, visit func(os.DirEntry) error) error {
	file, err := os.OpenFile(directory, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	for {
		entries, err := file.ReadDir(128)
		for _, entry := range entries {
			if err := visit(entry); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func historyPaths(root string) ([]string, error) {
	var paths []string
	seen := map[string]bool{}
	examined := 0
	var visit func(string, bool) error
	visit = func(directory string, descend bool) error {
		return walkHistoryDirectory(directory, func(entry os.DirEntry) error {
			examined++
			if examined > 10000 {
				return errHistoryScanLimit
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if entry.IsDir() {
				if descend {
					return visit(filepath.Join(directory, entry.Name()), false)
				}
				return nil
			}
			name := entry.Name()
			if offset := strings.LastIndex(name, ".log.edc."); offset >= 0 {
				if n, err := strconv.Atoi(name[offset+len(".log.edc."):]); err == nil && n > 0 {
					name = name[:offset+4]
				}
			}
			path := filepath.Join(directory, name)
			if strings.HasSuffix(name, ".log") && !seen[path] {
				if len(paths) >= historyFamilyLimit {
					return errHistoryScanLimit
				}
				seen[path] = true
				paths = append(paths, path)
			}
			return nil
		})
	}
	err := visit(root, true)
	return paths, err
}

func collectLogHistory(paths []string) logHistorySnapshot {
	snapshot := logHistorySnapshot{}
	unique := map[string]bool{}
	var ordered []string
	for _, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			snapshot.issue(path, "read_error")
			continue
		}
		if !unique[absolute] {
			unique[absolute] = true
			ordered = append(ordered, absolute)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ordered)))
	budget := int64(historyByteLimit)
	for index, path := range ordered {
		if index >= historyFamilyLimit {
			snapshot.issue(path, "limit_error")
			break
		}
		parts, err := historyFamily(path)
		if err != nil {
			if errors.Is(err, errHistoryScanLimit) {
				snapshot.issue(path, "limit_error")
				break
			}
			snapshot.issue(path, "read_error")
			continue
		}
		var size int64
		for _, part := range parts {
			if part.info.Size() > budget-size {
				size = budget + 1
				break
			}
			size += part.info.Size()
		}
		if size > budget {
			snapshot.issue(path, "limit_error")
			break
		}
		budget -= size
		rows, invalid, err := readHistoryFamily(path, parts)
		if err != nil {
			if errors.Is(err, errHistoryScanLimit) {
				snapshot.issue(path, "limit_error")
				break
			}
			snapshot.issue(path, "read_error")
			continue
		}
		if len(snapshot.Rows)+len(rows) > historyAttemptLimit {
			snapshot.issue(path, "limit_error")
			break
		}
		snapshot.Rows = append(snapshot.Rows, rows...)
		if invalid {
			snapshot.issue(path, "invalid_log")
		}
	}
	sort.SliceStable(snapshot.Rows, func(i, j int) bool {
		a, b := snapshot.Rows[i], snapshot.Rows[j]
		if !a.Started.Equal(b.Started) {
			return a.Started.After(b.Started)
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Ordinal > b.Ordinal
	})
	return snapshot
}

func historyCommand(row logHistoryAttempt) string {
	if len(row.Command) == 0 {
		return T("cli.log_history.hidden")
	}
	parts := make([]string, len(row.Command))
	for i, arg := range row.Command {
		if arg != "" && !strings.ContainsAny(arg, " \t\r\n\"'\\") {
			parts[i] = reportIdentityValue(arg)
		} else {
			parts[i] = strconv.QuoteToGraphic(arg)
		}
	}
	text := strings.Join(parts, " ")
	if row.Display == "name" || row.Display == "unknown" {
		text += " " + T("cli.log_history.args_unknown")
	}
	return text
}

func historyOutcome(row logHistoryAttempt) string {
	return T("cli.log_history.outcome." + strings.ToLower(row.Outcome))
}

func historyTime(at time.Time) string {
	if at.IsZero() {
		return "—"
	}
	return at.Local().Format("2006-01-02 15:04:05 MST")
}

func historyDuration(row logHistoryAttempt) string {
	if row.Ended.IsZero() || row.damaged {
		return "—"
	}
	return fmt.Sprint(row.Duration)
}
