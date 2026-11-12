package edc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestScanAuditHistoryFileFindsOnlySettingsThatTurnHistoryOff(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/home/deploy/.bashrc", strings.Join([]string{
		"export HISTSIZE=0",
		"HISTFILESIZE='0'",
		"HISTSIZE=0100",
		"# HISTSIZE=0",
		"  unset HISTFILE",
		"set +o history",
		"echo HISTSIZE=0",
		"PS1='# '; SAVEHIST=0 # keep nothing",
		"declare -x HISTSIZE=0",
		"export HISTFILE=/dev/null",
		"HISTFILE=~/.bash_history",
	}, "\n")+"\n", 0o644)
	settings, histfile, found, _, err := scanAuditHistoryFile(filepath.Join(root, "/home/deploy/.bashrc"), "/home/deploy/.bashrc")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, setting := range settings {
		got = append(got, fmt.Sprintf("%s@%d", setting.Statement, setting.Line))
	}
	want := []string{"HISTSIZE=0@1", "HISTFILESIZE=0@2", "unset HISTFILE@5", "set +o history@6", "SAVEHIST=0@8", "HISTSIZE=0@9", "HISTFILE=/dev/null@10"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("settings = %v", got)
	}
	// 마지막 대입이 실제로 쓰이는 HISTFILE이다.
	if !found || histfile != "~/.bash_history" {
		t.Fatalf("histfile = %q, found = %v", histfile, found)
	}
}

func TestAuditHistoryPath(t *testing.T) {
	for _, tc := range []struct {
		fallback, histfile string
		set                bool
		path, reason       string
	}{
		{fallback: ".bash_history", path: "/home/a/.bash_history"},
		{fallback: "", reason: "zsh_no_histfile"},
		{histfile: "~/.bash_history", set: true, path: "/home/a/.bash_history"},
		{histfile: "~/.zsh_history", set: true, path: "/home/a/.zsh_history"},
		{histfile: "$HOME/.zsh_history", set: true, path: "/home/a/.zsh_history"},
		{histfile: "${HOME}/h/zsh", set: true, path: "/home/a/h/zsh"},
		{histfile: "/dev/null", set: true, reason: "histfile_off"},
		{histfile: "", set: true, reason: "histfile_off"},
		{histfile: "$XDG_STATE_HOME/zsh/history", set: true, reason: "histfile_custom"},
		{histfile: "relative", set: true, reason: "histfile_custom"},
	} {
		path, reason := auditHistoryPath(tc.fallback, tc.histfile, tc.set, "/home/a")
		if path != tc.path || reason != tc.reason {
			t.Errorf("%+v: path = %q, reason = %q", tc, path, reason)
		}
	}
}

func TestCollectAuditHistoryAccounts(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/etc/passwd", strings.Join([]string{
		"root:x:0:0:root:/root:/bin/bash",
		"svc:x:998:998::/var/lib/svc:/usr/sbin/nologin",
		"deploy:x:1000:1000::/home/deploy:/bin/bash",
		"ops:x:1001:1001::/home/ops:/usr/bin/zsh",
		"dev:x:1002:1002::/home/dev:/bin/zsh",
		"fish:x:1003:1003::/home/fish:/usr/bin/fish",
	}, "\n")+"\n", 0o644)
	writeAuditFile(t, root, "/root/.bash_history", "ls\n", 0o600)
	if err := os.MkdirAll(filepath.Join(root, "home/deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", filepath.Join(root, "home/deploy/.bash_history")); err != nil {
		t.Fatal(err)
	}
	writeAuditFile(t, root, "/home/dev/.zshrc", "HISTFILE=~/.zsh_history\n", 0o644)
	writeAuditFile(t, root, "/home/ops/.zshrc", "export SAVEHIST=0\n", 0o644)
	passwd, err := readAuditPasswd(filepath.Join(root, "/etc/passwd"))
	if err != nil {
		t.Fatal(err)
	}
	history := collectAuditHistory(auditSources{root: root}, passwd)
	byUser := map[string]auditHistoryAccount{}
	for _, account := range history.Accounts {
		byUser[account.User] = account
	}
	if len(byUser) != 4 {
		t.Fatalf("accounts = %+v", history.Accounts)
	}
	if account := byUser["root"]; account.Path != "/root/.bash_history" || !account.Exists || account.Size != 3 {
		t.Fatalf("root = %+v", account)
	}
	if account := byUser["deploy"]; account.Target != "/dev/null" {
		t.Fatalf("deploy = %+v", account)
	}
	if account := byUser["ops"]; account.NotJudged != "zsh_no_histfile" {
		t.Fatalf("ops = %+v", account)
	}
	if account := byUser["dev"]; account.Path != "/home/dev/.zsh_history" || account.Exists {
		t.Fatalf("dev = %+v", account)
	}
	if len(history.Settings) != 1 || history.Settings[0].Path != "/home/ops/.zshrc" {
		t.Fatalf("settings = %+v", history.Settings)
	}
}

func testHistoryFacts(accounts []auditHistoryAccount, sessions []auditLogin) auditFacts {
	return auditFacts{
		Logins:  auditLoginRecords{Sessions: sessions, Since: testLoginAt(0)},
		History: auditHistory{Accounts: accounts},
	}
}

func TestAuditHistoryTamperedFailsOnRedirection(t *testing.T) {
	facts := testHistoryFacts([]auditHistoryAccount{
		{User: "root", Shell: "bash", Path: "/root/.bash_history", Exists: true, Size: 10, ModTime: testLoginAt(50)},
		{User: "deploy", Shell: "bash", Path: "/home/deploy/.bash_history", Exists: true, Target: "/dev/null", CharDevice: true},
	}, nil)
	result := auditHistoryTampered(facts)
	if result.Status != StatusFail || result.Next != "ls -la /home/deploy/.bash_history" || !strings.Contains(result.Evidence[0].Value, "-> /dev/null") {
		t.Fatalf("result = %+v", result)
	}
}

func TestAuditHistoryTamperedComparesEndedSessions(t *testing.T) {
	long := auditLogin{User: "deploy", Line: "pts/0", Host: "203.0.113.7", Start: testLoginAt(10), End: testLoginAt(15)}
	short := auditLogin{User: "deploy", Line: "pts/0", Start: testLoginAt(10), End: testLoginAt(10).Add(30 * time.Second)}
	open := auditLogin{User: "deploy", Line: "pts/0", Start: testLoginAt(10)}
	for name, tc := range map[string]struct {
		account auditHistoryAccount
		session auditLogin
		status  Status
		warning string
	}{
		"empty after a long session":   {auditHistoryAccount{Exists: true, Size: 0}, long, StatusFail, ""},
		"missing after a long session": {auditHistoryAccount{}, long, StatusFail, ""},
		"older than the session start": {auditHistoryAccount{Exists: true, Size: 5, ModTime: testLoginAt(5)}, long, StatusWarn, ""},
		// 로그아웃 뒤에 남은 process가 세션의 끝을 늦춰도, 세션 동안 쓴 history는 정상이다.
		"written during the session": {auditHistoryAccount{Exists: true, Size: 5, ModTime: testLoginAt(12)}, long, StatusPass, ""},
		"only a short session":       {auditHistoryAccount{}, short, StatusPass, T("audit.history.short_only", 1)},
		"no logout record":           {auditHistoryAccount{}, open, StatusPass, T("audit.history.no_logout", 1)},
		"console session only":       {auditHistoryAccount{}, auditLogin{User: "deploy", Line: "tty7", Start: testLoginAt(10), End: testLoginAt(20)}, StatusPass, ""},
	} {
		t.Run(name, func(t *testing.T) {
			account := tc.account
			account.User, account.Shell, account.Path = "deploy", "bash", "/home/deploy/.bash_history"
			result := auditHistoryTampered(testHistoryFacts([]auditHistoryAccount{account}, []auditLogin{tc.session}))
			if result.Status != tc.status {
				t.Fatalf("result = %+v", result)
			}
			if !strings.Contains(result.Summary, auditLoginTime(testLoginAt(0))) {
				t.Fatalf("summary has no range: %q", result.Summary)
			}
			if tc.warning != "" && !reflect.DeepEqual(result.Warnings, []string{tc.warning}) {
				t.Fatalf("warnings = %v", result.Warnings)
			}
		})
	}
}

func TestAuditHistoryTamperedReportsAccountsItDidNotJudge(t *testing.T) {
	facts := testHistoryFacts([]auditHistoryAccount{
		{User: "ops", Shell: "zsh", NotJudged: "zsh_no_histfile"},
		{User: "dev", Shell: "zsh", NotJudged: "histfile_custom"},
		{User: "qa", Shell: "bash", NotJudged: "histfile_off"},
	}, nil)
	facts.LoginErr = errors.New("unreadable")
	facts.History.Skipped = 2
	result := auditHistoryTampered(facts)
	want := []string{
		T("audit.history.no_logins"),
		T("audit.history.not_judged.zsh_no_histfile", "ops"),
		T("audit.history.not_judged.histfile_custom", "dev"),
		T("audit.history.not_judged.histfile_off", "qa"),
		T("audit.history.skipped", 2),
	}
	if result.Status != StatusPass || !reflect.DeepEqual(result.Warnings, want) {
		t.Fatalf("result = %+v", result)
	}
	for _, warning := range result.Warnings {
		if strings.HasPrefix(warning, "audit.") {
			t.Fatalf("missing message: %q", warning)
		}
	}
}

func TestAuditHistoryDisabled(t *testing.T) {
	facts := auditFacts{History: auditHistory{Settings: []auditHistorySetting{{Path: "/home/ops/.bashrc", Line: 42, Statement: "HISTSIZE=0"}}}}
	result := auditHistoryDisabled(facts)
	if result.Status != StatusWarn || result.Evidence[0].Value != "/home/ops/.bashrc:42  HISTSIZE=0" {
		t.Fatalf("result = %+v", result)
	}
	if result := auditHistoryDisabled(auditFacts{}); result.Status != StatusPass {
		t.Fatalf("result = %+v", result)
	}
}

func TestCollectAuditHistoryJudgesOnlyRegularFiles(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/etc/passwd", strings.Join([]string{
		"app1:x:1001:1001::/srv/shared:/bin/bash",
		"app2:x:1002:1002::/srv/shared:/bin/bash",
		"deploy:x:1000:1000::/home/deploy:/bin/bash",
		"ops:x:1003:1003::/home/ops:/bin/bash",
	}, "\n")+"\n", 0o644)
	writeAuditFile(t, root, "/srv/shared/.bash_history", "ls\n", 0o600)
	if err := os.MkdirAll(filepath.Join(root, "home/deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent/history", filepath.Join(root, "home/deploy/.bash_history")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "home/ops/.bash_history"), 0o755); err != nil {
		t.Fatal(err)
	}
	passwd, err := readAuditPasswd(filepath.Join(root, "/etc/passwd"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, account := range collectAuditHistory(auditSources{root: root}, passwd).Accounts {
		got = append(got, fmt.Sprintf("%s:%v:%s", account.User, account.Exists, account.Target))
	}
	// 홈을 함께 쓰는 계정은 각자 판정한다. 끊어진 symlink와 디렉터리는 history가 없는 것이다.
	if !reflect.DeepEqual(got, []string{"app1:true:", "app2:true:", "deploy:false:/nonexistent/history", "ops:false:"}) {
		t.Fatalf("accounts = %v", got)
	}
}

func TestAuditHistoryTamperedDoesNotFailWithoutLogins(t *testing.T) {
	facts := testHistoryFacts([]auditHistoryAccount{{User: "deploy", Shell: "bash", Path: "/home/deploy/.bash_history"}}, nil)
	facts.LoginErr = errors.New("unreadable")
	result := auditHistoryTampered(facts)
	if result.Status != StatusPass || !reflect.DeepEqual(result.Warnings, []string{T("audit.history.no_logins")}) {
		t.Fatalf("result = %+v", result)
	}
}

func TestCollectAuditHistoryDoesNotJudgeAccountsThatTurnHistoryOff(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/etc/passwd", "deploy:x:1000:1000::/home/deploy:/bin/bash\nops:x:1001:1001::/home/ops:/bin/zsh\nqa:x:1002:1002::/home/qa:/bin/bash\ndev:x:1004:1004::/home/dev:/bin/zsh\nsh:x:1005:1005::/home/sh:/bin/bash\n", 0o644)
	writeAuditFile(t, root, "/home/deploy/.bashrc", "HISTFILESIZE=0\n", 0o644)
	writeAuditFile(t, root, "/home/qa/.bashrc", "HISTFILE=~/.bash_history\n", 0o644)
	writeAuditFile(t, root, "/etc/zsh/zshrc", "HISTFILE=~/.zsh_history\nSAVEHIST=0\n", 0o644)
	// 나중에 읽는 사용자 파일이 다시 켜면 판정한다.
	writeAuditFile(t, root, "/home/dev/.zshrc", "SAVEHIST=1000\n", 0o644)
	writeAuditFile(t, root, "/home/sh/.bashrc", "set +o history\nset -o history\n", 0o644)
	passwd, err := readAuditPasswd(filepath.Join(root, "/etc/passwd"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, account := range collectAuditHistory(auditSources{root: root}, passwd).Accounts {
		got = append(got, account.User+":"+account.NotJudged)
	}
	if !reflect.DeepEqual(got, []string{"deploy:history_off", "ops:history_off", "qa:", "dev:", "sh:"}) {
		t.Fatalf("accounts = %v", got)
	}
	if message := T("audit.history.not_judged.history_off", "deploy"); strings.HasPrefix(message, "audit.") {
		t.Fatalf("missing message: %q", message)
	}
}
