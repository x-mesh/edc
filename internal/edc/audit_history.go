package edc

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// auditHistoryMinSession은 빈 history를 fail로 보는 세션 길이의 하한이다. Ctrl-D로 바로 끝낸 세션은 history에 아무것도 쓰지 않는다.
const auditHistoryMinSession = time.Minute

// auditHistoryShells는 계정의 shell마다 기본 history 파일과 시작 파일이다. 다른 shell은 history 형식과 위치가 달라 판정하지 않는다.
var auditHistoryShells = map[string]struct {
	history       string
	global, local []string
}{
	"bash": {
		history: ".bash_history",
		global:  []string{"/etc/profile", "/etc/profile.d/*.sh", "/etc/bash.bashrc", "/etc/bashrc"},
		local:   []string{".bash_profile", ".bash_login", ".profile", ".bashrc"},
	},
	"zsh": {
		global: []string{"/etc/zshenv", "/etc/zsh/zshenv", "/etc/zprofile", "/etc/zsh/zprofile", "/etc/zshrc", "/etc/zsh/zshrc"},
		local:  []string{".zshenv", ".zprofile", ".zshrc"},
	},
}

// auditHistoryLimits는 0이면 history를 남기지 않는 변수다.
var auditHistoryLimits = []string{"HISTSIZE", "HISTFILESIZE", "SAVEHIST"}

type auditHistorySetting struct {
	// Path는 시작 파일, Line은 1부터 센 줄 번호다. Statement는 줄 전체가 아니라 찾은 설정만 담는다. 시작 파일에는 secret이 있을 수 있다.
	Path      string
	Line      int
	Statement string
}

// auditHistoryToggle은 history를 끄거나 다시 켜는 대입 하나다. Name은 변수 이름이나 "set history"다.
type auditHistoryToggle struct {
	Name string
	Off  bool
}

type auditHistoryAccount struct {
	User, Shell string
	// Path는 history 파일이다. 비어 있으면 판정하지 않았고, 그 이유는 NotJudged에 있다.
	Path      string
	NotJudged string
	Exists    bool
	Size      int64
	ModTime   time.Time
	// Target은 symlink의 대상이고 CharDevice는 /dev/null 같은 장치 파일인지다.
	Target     string
	CharDevice bool
}

type auditHistory struct {
	Accounts []auditHistoryAccount
	Settings []auditHistorySetting
	// Skipped는 권한이 없어 읽지 못한 홈 디렉터리 수다.
	Skipped int
}

func collectAuditHistory(sources auditSources, passwd []auditPasswd) auditHistory {
	var history auditHistory
	seenFile := map[string]bool{}
	// scan은 시작 파일 하나에서 history 설정을 찾고 HISTFILE의 마지막 값을 돌려준다. 같은 파일은 한 번만 근거에 넣는다.
	// state에는 HISTSIZE 같은 변수와 set -o history가 history를 끄는지를 읽은 순서대로 덮어쓴다. 나중 파일이 다시 켜면 그 값을 따른다.
	scan := func(path string, state map[string]bool) (string, bool, error) {
		settings, histfile, found, toggles, err := scanAuditHistoryFile(sources.file(path), path)
		if err == nil && !seenFile[path] {
			seenFile[path] = true
			history.Settings = append(history.Settings, settings...)
		}
		for _, toggle := range toggles {
			state[toggle.Name] = toggle.Off
		}
		return histfile, found, err
	}
	globals := map[string]string{}
	globalState := map[string]map[string]bool{}
	for shell, spec := range auditHistoryShells {
		for _, pattern := range spec.global {
			matches, _ := filepath.Glob(sources.file(pattern))
			slices.Sort(matches)
			for _, match := range matches {
				path := "/" + strings.TrimPrefix(filepath.ToSlash(strings.TrimPrefix(match, filepath.Clean(sources.root))), "/")
				if globalState[shell] == nil {
					globalState[shell] = map[string]bool{}
				}
				value, found, err := scan(path, globalState[shell])
				if err == nil && found {
					globals[shell] = value
				}
			}
		}
	}
	// 홈을 함께 쓰는 계정도 각자의 세션과 비교한다. 한 계정만 보면 다른 계정이 비운 history를 놓친다.
	for _, entry := range passwd {
		shell := filepath.Base(entry.Shell)
		spec, ok := auditHistoryShells[shell]
		if (entry.UID != 0 && entry.UID < 1000) || !ok || entry.Home == "" || entry.Home == "/" {
			continue
		}
		account := auditHistoryAccount{User: entry.Name, Shell: shell}
		histfile, set := globals[shell]
		state := maps.Clone(globalState[shell])
		if state == nil {
			state = map[string]bool{}
		}
		denied := false
		for _, name := range spec.local {
			value, found, err := scan(filepath.Join(entry.Home, name), state)
			if errors.Is(err, fs.ErrPermission) {
				denied = true
				break
			}
			if found {
				histfile, set = value, true
			}
		}
		if denied {
			history.Skipped++
			continue
		}
		path, reason := auditHistoryPath(spec.history, histfile, set, entry.Home)
		// history를 끄는 설정이 있으면 빈 history는 그 설정 때문일 수 있다. 설정은 history.disabled가 알린다.
		if reason == "" && slices.Contains(slices.Collect(maps.Values(state)), true) {
			reason = "history_off"
		}
		if reason != "" {
			account.NotJudged = reason
			history.Accounts = append(history.Accounts, account)
			continue
		}
		account.Path = path
		info, err := os.Lstat(sources.file(path))
		if errors.Is(err, fs.ErrPermission) {
			history.Skipped++
			continue
		}
		if err == nil {
			if info.Mode()&fs.ModeSymlink != 0 {
				account.Target, _ = os.Readlink(sources.file(path))
				// 끊어진 symlink는 history가 없는 것이다. 링크 자체의 크기와 시각을 쓰면 빈 history를 놓친다.
				info, err = os.Stat(sources.file(path))
				if errors.Is(err, fs.ErrPermission) {
					history.Skipped++
					continue
				}
			}
		}
		if err == nil {
			account.CharDevice = info.Mode()&fs.ModeCharDevice != 0
			// 디렉터리처럼 일반 파일이 아닌 것에는 shell이 history를 쓰지 못한다.
			account.Exists = info.Mode().IsRegular()
			if account.Exists {
				account.Size = info.Size()
				account.ModTime = info.ModTime()
			}
		}
		history.Accounts = append(history.Accounts, account)
	}
	return history
}

// auditHistoryPath는 계정의 history 파일을 정한다. HISTFILE이 기본과 다른 경로면 shell 변수를 펼쳐야 해 판정하지 않는다.
func auditHistoryPath(fallback, histfile string, set bool, home string) (string, string) {
	if !set {
		if fallback == "" {
			return "", "zsh_no_histfile"
		}
		return filepath.Join(home, fallback), ""
	}
	if histfile == "" || histfile == "/dev/null" {
		return "", "histfile_off"
	}
	for _, prefix := range []string{"~/", "$HOME/", "${HOME}/"} {
		if rest, ok := strings.CutPrefix(histfile, prefix); ok {
			histfile = filepath.Join(home, rest)
			break
		}
	}
	if !filepath.IsAbs(histfile) || strings.ContainsAny(histfile, "$`*?") {
		return "", "histfile_custom"
	}
	return filepath.Clean(histfile), ""
}

// scanAuditHistoryFile은 history를 끄는 설정을 찾는다. 조건문 안의 설정도 실행된다고 본다. shell을 해석하지 않으므로 어느 분기인지 알 수 없다.
func scanAuditHistoryFile(full, path string) ([]auditHistorySetting, string, bool, []auditHistoryToggle, error) {
	file, err := os.Open(full)
	if err != nil {
		return nil, "", false, nil, err
	}
	defer file.Close()
	var settings []auditHistorySetting
	var toggles []auditHistoryToggle
	histfile, found := "", false
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for number := 1; scanner.Scan(); number++ {
		for _, statement := range auditShellStatements(scanner.Text()) {
			fields := strings.Fields(statement)
			if len(fields) == 0 {
				continue
			}
			switch {
			case fields[0] == "unset" && slices.Contains(fields[1:], "HISTFILE"):
				settings = append(settings, auditHistorySetting{Path: path, Line: number, Statement: "unset HISTFILE"})
				histfile, found = "", true
			case fields[0] == "set" && len(fields) >= 3 && fields[1] == "+o" && fields[2] == "history":
				settings = append(settings, auditHistorySetting{Path: path, Line: number, Statement: "set +o history"})
				toggles = append(toggles, auditHistoryToggle{Name: "set history", Off: true})
			case fields[0] == "set" && len(fields) >= 3 && fields[1] == "-o" && fields[2] == "history":
				toggles = append(toggles, auditHistoryToggle{Name: "set history"})
			default:
				for _, field := range auditAssignmentFields(fields) {
					name, value, ok := strings.Cut(field, "=")
					if !ok {
						continue
					}
					value = strings.Trim(value, `"'`)
					switch {
					case name == "HISTFILE":
						histfile, found = value, true
						if value == "/dev/null" || value == "" {
							settings = append(settings, auditHistorySetting{Path: path, Line: number, Statement: fmt.Sprintf("HISTFILE=%s", value)})
						}
					case slices.Contains(auditHistoryLimits, name):
						toggles = append(toggles, auditHistoryToggle{Name: name, Off: value == "0"})
						if value == "0" {
							settings = append(settings, auditHistorySetting{Path: path, Line: number, Statement: name + "=0"})
						}
					}
				}
			}
		}
	}
	return settings, histfile, found, toggles, scanner.Err()
}

// auditShellStatements는 주석을 지우고 ;로 나눈 명령이다. # 앞이 공백일 때만 주석이다. 값 안의 #은 주석이 아니다.
func auditShellStatements(line string) []string {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") {
		return nil
	}
	if index := strings.Index(trimmed, " #"); index >= 0 {
		trimmed = trimmed[:index]
	}
	return strings.Split(trimmed, ";")
}

// auditAssignmentFields는 export, declare, typeset, local 뒤의 NAME=VALUE를 돌려준다. 다른 명령의 인자는 대입이 아니다.
func auditAssignmentFields(fields []string) []string {
	switch fields[0] {
	case "export", "declare", "typeset", "local", "readonly":
		var assignments []string
		for _, field := range fields[1:] {
			if !strings.HasPrefix(field, "-") {
				assignments = append(assignments, field)
			}
		}
		return assignments
	}
	if strings.Contains(fields[0], "=") {
		return fields[:1]
	}
	return nil
}

type auditHistorySessions struct {
	// Ended는 1분 넘게 이어지다 끝난 마지막 세션이다. HasEnded가 false면 비어 있다.
	Ended               auditLogin
	HasEnded            bool
	HasLogin, HasLogout bool
}

func auditHistoryEndedSession(records auditLoginRecords, user string) auditHistorySessions {
	var sessions auditHistorySessions
	for _, session := range records.Sessions {
		// 화면 로그인(:0, tty7)과 콘솔은 bash login shell을 거치지 않거나 history를 다른 shell이 쓴다. ssh와 터미널의 pty만 비교한다.
		if session.User != user || !strings.HasPrefix(session.Line, "pts/") {
			continue
		}
		sessions.HasLogin = true
		if session.End.IsZero() {
			continue
		}
		sessions.HasLogout = true
		if session.End.Sub(session.Start) > auditHistoryMinSession && (!sessions.HasEnded || session.End.After(sessions.Ended.End)) {
			sessions.Ended, sessions.HasEnded = session, true
		}
	}
	return sessions
}

func auditHistoryTampered(facts auditFacts) Result {
	const id = "history.tampered"
	if facts.PasswdErr != nil {
		return auditSkip(id, "/etc/passwd", facts.PasswdErr)
	}
	loginsKnown := facts.LoginErr == nil
	var fails, warns, notJudged []string
	var firstPath string
	judged, noLogout, shortOnly := 0, 0, 0
	for _, account := range facts.History.Accounts {
		if account.NotJudged != "" {
			notJudged = append(notJudged, T("audit.history.not_judged."+account.NotJudged, account.User))
			continue
		}
		judged++
		if account.Target == "/dev/null" || account.CharDevice {
			target := account.Target
			if target == "" {
				target = T("audit.history.char_device")
			}
			fails = append(fails, fmt.Sprintf("%s  %s -> %s", account.User, account.Path, target))
			firstPath = cmp.Or(firstPath, account.Path)
			continue
		}
		if !loginsKnown {
			continue
		}
		sessions := auditHistoryEndedSession(facts.Logins, account.User)
		switch {
		case sessions.HasEnded:
		case sessions.HasLogout:
			shortOnly++
			continue
		case sessions.HasLogin:
			noLogout++
			continue
		default:
			continue
		}
		ended := sessions.Ended
		switch {
		case !account.Exists || account.Size == 0:
			firstPath = cmp.Or(firstPath, account.Path)
			fails = append(fails, auditItem(account.User+"  "+account.Path, nil, []string{T("audit.history.empty_after", auditLoginTime(ended.Start), auditLoginTime(ended.End), ended.Host)}))
		// 세션 동안 shell이 하나라도 끝나면 history를 쓴다. 끝 시각과 비교하면 로그아웃 뒤에 남은 process 때문에 오탐이 나므로 시작 시각과 비교한다.
		case account.ModTime.Before(ended.Start):
			warns = append(warns, auditItem(account.User+"  "+account.Path, []string{T("audit.history.modified", auditLoginTime(account.ModTime))},
				[]string{T("audit.history.older_than", auditLoginTime(ended.Start), auditLoginTime(ended.End))}))
		}
	}
	var result Result
	switch {
	case len(fails) > 0:
		result = auditResult(id, StatusFail, T("audit.history.tampered_bad", len(fails)))
		result.Error = &DiagnosticError{Kind: "history", Message: result.Summary}
		result.Evidence = []Evidence{{Label: T("audit.evidence.accounts"), Value: strings.Join(append(fails, warns...), "\n")}}
		result.Next = "ls -la " + firstPath
	case len(warns) > 0:
		result = auditResult(id, StatusWarn, T("audit.history.older_bad", len(warns)))
		result.Evidence = []Evidence{{Label: T("audit.evidence.accounts"), Value: strings.Join(warns, "\n")}}
	default:
		result = auditResult(id, StatusPass, T("audit.history.tampered_ok", judged))
	}
	if !loginsKnown {
		result.Warnings = append(result.Warnings, T("audit.history.no_logins"))
	} else if !facts.Logins.Since.IsZero() {
		result.Summary += " " + T("audit.history.since", auditLoginTime(facts.Logins.Since))
	}
	if noLogout > 0 {
		result.Warnings = append(result.Warnings, T("audit.history.no_logout", noLogout))
	}
	if shortOnly > 0 {
		result.Warnings = append(result.Warnings, T("audit.history.short_only", shortOnly))
	}
	result.Warnings = append(result.Warnings, notJudged...)
	if facts.History.Skipped > 0 {
		result.Warnings = append(result.Warnings, T("audit.history.skipped", facts.History.Skipped))
	}
	if facts.ExternalAccounts != "" {
		result.Warnings = append(result.Warnings, T("audit.account.external", facts.ExternalAccounts))
	}
	return result
}

func auditHistoryDisabled(facts auditFacts) Result {
	const id = "history.disabled"
	if facts.PasswdErr != nil {
		return auditSkip(id, "/etc/passwd", facts.PasswdErr)
	}
	var result Result
	if len(facts.History.Settings) == 0 {
		result = auditResult(id, StatusPass, T("audit.history.disabled_ok"))
	} else {
		lines := make([]string, 0, len(facts.History.Settings))
		for _, setting := range facts.History.Settings {
			lines = append(lines, fmt.Sprintf("%s:%d  %s", setting.Path, setting.Line, setting.Statement))
		}
		result = auditResult(id, StatusWarn, T("audit.history.disabled_bad", len(lines)))
		result.Evidence = []Evidence{{Label: T("audit.evidence.settings"), Value: strings.Join(lines, "\n")}}
	}
	if facts.History.Skipped > 0 {
		result.Warnings = []string{T("audit.history.skipped", facts.History.Skipped)}
	}
	return result
}
