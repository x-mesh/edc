package edc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// cleanGitEnvironment는 사용자의 전역 git 설정이 시험에 섞이지 않게 한다.
func cleanGitEnvironment(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	values := map[string]string{
		"HOME": t.TempDir(), "GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_SYSTEM": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "edc test", "GIT_AUTHOR_EMAIL": "edc@example.invalid",
		"GIT_COMMITTER_NAME": "edc test", "GIT_COMMITTER_EMAIL": "edc@example.invalid", "GIT_TERMINAL_PROMPT": "0",
	}
	for name, value := range values {
		t.Setenv(name, value)
	}
	// git hook 안에서 시험을 돌리면 이 변수들이 저장소를 가리킨다.
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OPTIONAL_LOCKS"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

func cleanGitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

type cleanWorktreeLab struct {
	t    *testing.T
	home string
	root string
}

func newCleanWorktreeLab(t *testing.T) *cleanWorktreeLab {
	t.Helper()
	cleanGitEnvironment(t)
	home := cleanTestHome(t)
	return &cleanWorktreeLab{t: t, home: home, root: filepath.Join(home, ".gk", "worktree")}
}

// repo는 main 브랜치에 커밋 하나가 있는 저장소를 root 밖에 만든다.
func (lab *cleanWorktreeLab) repo(name string) string {
	lab.t.Helper()
	dir := filepath.Join(lab.home, "src", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		lab.t.Fatal(err)
	}
	cleanGitRun(lab.t, dir, "init", "-q", "-b", "main")
	lab.write(filepath.Join(dir, "tracked"), "one\n")
	cleanGitRun(lab.t, dir, "add", "tracked")
	cleanGitRun(lab.t, dir, "commit", "-q", "-m", "one")
	return dir
}

func (lab *cleanWorktreeLab) write(path, text string) {
	lab.t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		lab.t.Fatal(err)
	}
}

// add는 root 아래에 worktree를 만든다. start가 비면 main에서 시작한다.
func (lab *cleanWorktreeLab) add(repo, name, branch string, extra ...string) string {
	lab.t.Helper()
	path := filepath.Join(lab.root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		lab.t.Fatal(err)
	}
	args := append([]string{"worktree", "add", "-q", "-b", branch, path}, extra...)
	if len(extra) == 0 {
		args = append(args, "main")
	}
	cleanGitRun(lab.t, repo, args...)
	return path
}

func (lab *cleanWorktreeLab) deps(run func(context.Context, cleanToolSpec) cleanToolOutput) cleanDeps {
	if run == nil {
		run = runCleanTool
	}
	return cleanTestDeps(lab.home, run)
}

func (lab *cleanWorktreeLab) scan(run func(context.Context, cleanToolSpec) cleanToolOutput) cleanWorktrees {
	lab.t.Helper()
	return cleanWorktreeScan(context.Background(), lab.deps(run))
}

func (lab *cleanWorktreeLab) only(section cleanWorktrees) cleanWorktree {
	lab.t.Helper()
	if len(section.Items) != 1 {
		lab.t.Fatalf("items = %d, want 1: %+v", len(section.Items), section)
	}
	return section.Items[0]
}

// cleanGitSpy는 edc가 실행하는 git 명령을 기록하고, 필요하면 결과를 바꾼다.
type cleanGitSpy struct {
	mu       sync.Mutex
	calls    []cleanToolSpec
	override func(sub string, spec cleanToolSpec, output cleanToolOutput) cleanToolOutput
}

func cleanGitSub(spec cleanToolSpec) string {
	args := spec.Args
	for len(args) >= 2 && args[0] == "-c" {
		args = args[2:]
	}
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func (spy *cleanGitSpy) run(ctx context.Context, spec cleanToolSpec) cleanToolOutput {
	spy.mu.Lock()
	spy.calls = append(spy.calls, spec)
	spy.mu.Unlock()
	output := runCleanTool(ctx, spec)
	if spy.override != nil {
		output = spy.override(cleanGitSub(spec), spec, output)
	}
	return output
}

func (spy *cleanGitSpy) count(sub string) int {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	count := 0
	for _, call := range spy.calls {
		if cleanGitSub(call) == sub {
			count++
		}
	}
	return count
}

func cleanExit(code int) cleanToolOutput {
	return cleanToolOutput{Err: fmt.Errorf("exit status %d", code), ExitCode: code}
}

func TestCleanBoundaryWorktreeScanLeavesGitFilesUntouched(t *testing.T) {
	lab := newCleanWorktreeLab(t)
	repo := lab.repo("main")
	a := lab.add(repo, "a", "branch-a")
	b := lab.add(repo, "b", "branch-b")
	// tracked 파일의 내용은 그대로 두고 수정 시각만 바꾸면 일반 git status가 index를 다시 쓴다.
	old := time.Now().Add(-48 * time.Hour)
	for _, worktree := range []string{a, b} {
		if err := os.Chtimes(filepath.Join(worktree, "tracked"), old, old); err != nil {
			t.Fatal(err)
		}
	}
	fingerprint := func(name string) string {
		var parts []string
		for _, file := range []string{"index", "logs/HEAD"} {
			path := filepath.Join(repo, ".git", "worktrees", name, file)
			info, err := os.Stat(path)
			if errors.Is(err, os.ErrNotExist) {
				parts = append(parts, file+": absent")
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			parts = append(parts, fmt.Sprintf("%s: %d %d %s", file, info.Size(), info.ModTime().UnixNano(), hex.EncodeToString(sum[:])))
		}
		return strings.Join(parts, "\n")
	}

	// 양성 대조: 같은 상태에서 일반 git status는 index를 바꾼다. 바꾸지 않으면 아래 검사는 아무것도 증명하지 못한다.
	controlBefore := fingerprint("a")
	cleanGitRun(t, a, "status", "--porcelain")
	if fingerprint("a") == controlBefore {
		t.Fatal("a plain git status did not rewrite the index, so this test cannot detect a write")
	}

	beforeA, beforeB := fingerprint("a"), fingerprint("b")
	section := lab.scan(nil)
	if len(section.Items) != 2 {
		t.Fatalf("items = %+v", section.Items)
	}
	for _, item := range section.Items {
		if item.Changes == nil || item.Untracked == nil {
			t.Fatalf("git status did not run for %s: %+v", item.Path, item)
		}
	}
	if got := fingerprint("a"); got != beforeA {
		t.Errorf("index or logs/HEAD of a changed:\n%s\n%s", beforeA, got)
	}
	if got := fingerprint("b"); got != beforeB {
		t.Errorf("index or logs/HEAD of b changed:\n%s\n%s", beforeB, got)
	}
}

func TestCleanBoundaryWorktreeVerdicts(t *testing.T) {
	type expect struct {
		verdict    string
		kind       string
		base       string
		source     string
		merged     string
		changes    int
		untracked  int
		reason     string
		registered string
	}
	check := func(t *testing.T, item cleanWorktree, want expect) {
		t.Helper()
		if item.Verdict != want.verdict {
			t.Errorf("verdict = %q, want %q: %+v", item.Verdict, want.verdict, item)
		}
		if want.kind != "" && item.Kind != want.kind {
			t.Errorf("kind = %q, want %q", item.Kind, want.kind)
		}
		if item.Base != want.base || item.BaseSource != want.source {
			t.Errorf("base = %q (%s), want %q (%s)", item.Base, item.BaseSource, want.base, want.source)
		}
		merged := "null"
		if item.Merged != nil {
			merged = fmt.Sprint(*item.Merged)
		}
		if want.merged != "" && merged != want.merged {
			t.Errorf("merged = %s, want %s", merged, want.merged)
		}
		if want.changes >= 0 && (item.Changes == nil || *item.Changes != want.changes) {
			t.Errorf("changes = %v, want %d", item.Changes, want.changes)
		}
		if want.untracked >= 0 && (item.Untracked == nil || *item.Untracked != want.untracked) {
			t.Errorf("untracked = %v, want %d", item.Untracked, want.untracked)
		}
		if want.reason != "" && !slices.Contains(item.Reasons, want.reason) {
			t.Errorf("reasons = %v, want %s", item.Reasons, want.reason)
		}
		registered := "null"
		if item.Registered != nil {
			registered = fmt.Sprint(*item.Registered)
		}
		if want.registered != "" && registered != want.registered {
			t.Errorf("registered = %s, want %s", registered, want.registered)
		}
	}

	t.Run("merged into the git-kit parent", func(t *testing.T) {
		lab := newCleanWorktreeLab(t)
		repo := lab.repo("main")
		lab.add(repo, "w", "work")
		cleanGitRun(t, repo, "config", "branch.work.gk-parent", "main")
		item := lab.only(lab.scan(nil))
		check(t, item, expect{verdict: cleanVerdictMergedClean, kind: cleanWorktreeLinked, base: "main", source: cleanBaseGKParent, merged: "true",
			changes: 0, untracked: 0, registered: "true"})
		if item.Branch != "work" || item.Head == "" || item.LastCommitAt == nil || item.Ahead == nil || *item.Ahead != 0 {
			t.Errorf("item = %+v", item)
		}
		if item.Repository != filepath.Join(repo, ".git") {
			t.Errorf("repository = %q", item.Repository)
		}
	})
	t.Run("the git-kit parent beats origin/HEAD, and develop is not assumed to be main", func(t *testing.T) {
		lab := newCleanWorktreeLab(t)
		repo := lab.repo("main")
		first := lab.add(repo, "one", "work-one")
		lab.write(filepath.Join(first, "extra"), "x\n")
		cleanGitRun(t, first, "add", "extra")
		cleanGitRun(t, first, "commit", "-q", "-m", "extra")
		cleanGitRun(t, repo, "branch", "develop", "work-one")
		lab.add(repo, "two", "work-two", "work-one")
		cleanGitRun(t, repo, "update-ref", "refs/remotes/origin/main", "main")
		cleanGitRun(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
		cleanGitRun(t, repo, "config", "branch.work-one.gk-parent", "develop")
		section := lab.scan(nil)
		byName := map[string]cleanWorktree{}
		for _, item := range section.Items {
			byName[filepath.Base(item.Path)] = item
		}
		check(t, byName["one"], expect{verdict: cleanVerdictMergedClean, base: "develop", source: cleanBaseGKParent, merged: "true", changes: 0, untracked: 0})
		// 같은 커밋이어도 부모 설정이 없으면 origin/HEAD(main)와 비교하고, 거기에는 병합되지 않았다.
		check(t, byName["two"], expect{verdict: cleanVerdictNotMerged, base: "origin/main", source: cleanBaseOriginHead, merged: "false", changes: 0, untracked: 0})
		if ahead := byName["two"].Ahead; ahead == nil || *ahead != 1 {
			t.Errorf("ahead = %v", ahead)
		}
	})
	t.Run("a commit the parent lacks is not merged", func(t *testing.T) {
		lab := newCleanWorktreeLab(t)
		repo := lab.repo("main")
		worktree := lab.add(repo, "w", "work")
		cleanGitRun(t, repo, "config", "branch.work.gk-parent", "main")
		lab.write(filepath.Join(worktree, "extra"), "x\n")
		cleanGitRun(t, worktree, "add", "extra")
		cleanGitRun(t, worktree, "commit", "-q", "-m", "extra")
		item := lab.only(lab.scan(nil))
		check(t, item, expect{verdict: cleanVerdictNotMerged, base: "main", source: cleanBaseGKParent, merged: "false", changes: 0, untracked: 0})
		if item.Ahead == nil || *item.Ahead != 1 {
			t.Errorf("ahead = %v", item.Ahead)
		}
	})
	t.Run("origin/HEAD is the base without a git-kit parent", func(t *testing.T) {
		lab := newCleanWorktreeLab(t)
		repo := lab.repo("main")
		lab.add(repo, "w", "work")
		cleanGitRun(t, repo, "update-ref", "refs/remotes/origin/main", "main")
		cleanGitRun(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
		check(t, lab.only(lab.scan(nil)), expect{verdict: cleanVerdictMergedClean, base: "origin/main", source: cleanBaseOriginHead, merged: "true", changes: 0, untracked: 0})
	})
	t.Run("no base is base_unknown, not merged_clean", func(t *testing.T) {
		lab := newCleanWorktreeLab(t)
		lab.add(lab.repo("main"), "w", "work")
		check(t, lab.only(lab.scan(nil)), expect{verdict: cleanVerdictBaseUnknown, source: cleanBaseNone, merged: "null", changes: 0, untracked: 0})
	})
	t.Run("a git-kit parent branch that does not exist", func(t *testing.T) {
		lab := newCleanWorktreeLab(t)
		repo := lab.repo("main")
		lab.add(repo, "w", "work")
		cleanGitRun(t, repo, "config", "branch.work.gk-parent", "gone")
		check(t, lab.only(lab.scan(nil)), expect{verdict: cleanVerdictBaseUnknown, source: cleanBaseGKParent, merged: "null", changes: 0, untracked: 0, reason: cleanReasonBaseMissing})
	})
	t.Run("a git-kit parent that looks like an option is never passed to git", func(t *testing.T) {
		lab := newCleanWorktreeLab(t)
		repo := lab.repo("main")
		lab.add(repo, "w", "work")
		cleanGitRun(t, repo, "config", "branch.work.gk-parent", "--output=/dev/null")
		spy := &cleanGitSpy{}
		check(t, lab.only(lab.scan(spy.run)), expect{verdict: cleanVerdictBaseUnknown, source: cleanBaseGKParent, merged: "null", changes: 0, untracked: 0, reason: cleanReasonBaseMissing})
		for _, call := range spy.calls {
			if slices.Contains(call.Args, "--output=/dev/null") {
				t.Fatalf("git got the option-like base: %v", call.Args)
			}
		}
	})
	t.Run("a tracked change", func(t *testing.T) {
		lab := newCleanWorktreeLab(t)
		repo := lab.repo("main")
		worktree := lab.add(repo, "w", "work")
		cleanGitRun(t, repo, "config", "branch.work.gk-parent", "main")
		lab.write(filepath.Join(worktree, "tracked"), "changed\n")
		check(t, lab.only(lab.scan(nil)), expect{verdict: cleanVerdictHasChanges, base: "main", source: cleanBaseGKParent, merged: "true", changes: 1, untracked: 0})
	})
	t.Run("an untracked file", func(t *testing.T) {
		lab := newCleanWorktreeLab(t)
		repo := lab.repo("main")
		worktree := lab.add(repo, "w", "work")
		cleanGitRun(t, repo, "config", "branch.work.gk-parent", "main")
		lab.write(filepath.Join(worktree, "new"), "n\n")
		check(t, lab.only(lab.scan(nil)), expect{verdict: cleanVerdictHasChanges, base: "main", source: cleanBaseGKParent, merged: "true", changes: 0, untracked: 1})
	})
	t.Run("a locked worktree", func(t *testing.T) {
		lab := newCleanWorktreeLab(t)
		repo := lab.repo("main")
		worktree := lab.add(repo, "w", "work")
		cleanGitRun(t, repo, "config", "branch.work.gk-parent", "main")
		cleanGitRun(t, repo, "worktree", "lock", worktree)
		item := lab.only(lab.scan(nil))
		check(t, item, expect{verdict: cleanVerdictLocked, base: "main", source: cleanBaseGKParent, merged: "true", changes: 0, untracked: 0, registered: "true"})
		if !item.Locked {
			t.Error("locked = false")
		}
	})
	t.Run("a detached HEAD uses origin/HEAD", func(t *testing.T) {
		lab := newCleanWorktreeLab(t)
		repo := lab.repo("main")
		path := filepath.Join(lab.root, "w")
		cleanGitRun(t, repo, "worktree", "add", "-q", "--detach", path, "main")
		cleanGitRun(t, repo, "update-ref", "refs/remotes/origin/main", "main")
		cleanGitRun(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
		item := lab.only(lab.scan(nil))
		check(t, item, expect{verdict: cleanVerdictMergedClean, base: "origin/main", source: cleanBaseOriginHead, merged: "true", changes: 0, untracked: 0})
		if item.Branch != "" {
			t.Errorf("branch = %q", item.Branch)
		}
	})
	t.Run("a worktree whose repository record is gone is unlinked and git is not called", func(t *testing.T) {
		lab := newCleanWorktreeLab(t)
		repo := lab.repo("main")
		lab.add(repo, "w", "work")
		if err := os.RemoveAll(filepath.Join(repo, ".git", "worktrees")); err != nil {
			t.Fatal(err)
		}
		spy := &cleanGitSpy{}
		item := lab.only(lab.scan(spy.run))
		check(t, item, expect{verdict: cleanVerdictUnlinked, kind: cleanWorktreeUnlinked, source: cleanBaseNone, merged: "null", changes: -1, untracked: -1,
			reason: cleanReasonGitdirMissing, registered: "null"})
		if item.Changes != nil || item.Untracked != nil {
			t.Errorf("unlinked item carries counts: %+v", item)
		}
		if len(spy.calls) != 0 {
			t.Fatalf("git ran %d times: %v", len(spy.calls), spy.calls[0].Args)
		}
	})
	t.Run("a repository inside the root has no registration", func(t *testing.T) {
		lab := newCleanWorktreeLab(t)
		dir := filepath.Join(lab.root, "plain")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		cleanGitRun(t, dir, "init", "-q", "-b", "main")
		lab.write(filepath.Join(dir, "f"), "f\n")
		cleanGitRun(t, dir, "add", "f")
		cleanGitRun(t, dir, "commit", "-q", "-m", "one")
		check(t, lab.only(lab.scan(nil)), expect{verdict: cleanVerdictBaseUnknown, kind: cleanWorktreeRepository, source: cleanBaseNone, merged: "null", changes: 0, untracked: 0, registered: "null"})
	})

	// 명령이 답하지 못하면 merged_clean이 될 수 없다. 변경이 확인됐으면 has_changes다.
	clean := func(t *testing.T) (*cleanWorktreeLab, string) {
		lab := newCleanWorktreeLab(t)
		repo := lab.repo("main")
		worktree := lab.add(repo, "w", "work")
		cleanGitRun(t, repo, "config", "branch.work.gk-parent", "main")
		return lab, worktree
	}
	t.Run("one failing command is state_unknown", func(t *testing.T) {
		for _, sub := range []string{"rev-parse", "diff-index", "diff-files", "ls-files", "worktree", "config", "merge-base", "rev-list", "log"} {
			lab, _ := clean(t)
			spy := &cleanGitSpy{override: func(got string, spec cleanToolSpec, output cleanToolOutput) cleanToolOutput {
				if got == sub {
					return cleanExit(3)
				}
				return output
			}}
			item := lab.only(lab.scan(spy.run))
			if item.Verdict != cleanVerdictStateUnk || !slices.Contains(item.Reasons, cleanReasonToolFailed) {
				t.Errorf("%s fails: verdict = %q, reasons = %v", sub, item.Verdict, item.Reasons)
			}
		}
	})
	t.Run("a failing runner that cannot start git is state_unknown", func(t *testing.T) {
		lab, _ := clean(t)
		item := lab.only(lab.scan(func(context.Context, cleanToolSpec) cleanToolOutput {
			return cleanToolOutput{Err: errors.New("exec: not found")}
		}))
		if item.Verdict != cleanVerdictStateUnk || item.Changes != nil || item.Untracked != nil {
			t.Errorf("item = %+v", item)
		}
	})
	t.Run("a timeout is state_unknown", func(t *testing.T) {
		lab, _ := clean(t)
		spy := &cleanGitSpy{override: func(sub string, _ cleanToolSpec, output cleanToolOutput) cleanToolOutput {
			if sub == "merge-base" {
				return cleanToolOutput{Err: errCleanToolTimeout}
			}
			return output
		}}
		item := lab.only(lab.scan(spy.run))
		if item.Verdict != cleanVerdictStateUnk || !slices.Contains(item.Reasons, cleanReasonToolTimeout) {
			t.Errorf("item = %+v", item)
		}
	})
	t.Run("a failure after a confirmed change keeps has_changes", func(t *testing.T) {
		lab, worktree := clean(t)
		lab.write(filepath.Join(worktree, "new"), "n\n")
		spy := &cleanGitSpy{override: func(sub string, _ cleanToolSpec, output cleanToolOutput) cleanToolOutput {
			if sub == "merge-base" || sub == "log" {
				return cleanExit(128)
			}
			return output
		}}
		item := lab.only(lab.scan(spy.run))
		if item.Verdict != cleanVerdictHasChanges {
			t.Errorf("item = %+v", item)
		}
	})
	t.Run("a cut status keeps the change and marks the count a lower bound", func(t *testing.T) {
		lab, worktree := clean(t)
		lab.write(filepath.Join(worktree, "new"), "n\n")
		lab.write(filepath.Join(worktree, "new2"), "n\n")
		spy := &cleanGitSpy{override: func(sub string, _ cleanToolSpec, output cleanToolOutput) cleanToolOutput {
			if sub == "ls-files" {
				output.Truncated = true
			}
			return output
		}}
		item := lab.only(lab.scan(spy.run))
		if item.Verdict != cleanVerdictHasChanges || !item.CountsLowerBound || item.Untracked == nil || *item.Untracked != 1 ||
			!slices.Contains(item.Reasons, cleanReasonGitOutputTruncated) {
			t.Errorf("item = %+v", item)
		}
	})
	t.Run("a cut status without a counted line is not a clean tree", func(t *testing.T) {
		lab, _ := clean(t)
		spy := &cleanGitSpy{override: func(sub string, _ cleanToolSpec, output cleanToolOutput) cleanToolOutput {
			if sub == "ls-files" {
				output.Truncated = true
			}
			return output
		}}
		item := lab.only(lab.scan(spy.run))
		if item.Verdict != cleanVerdictStateUnk || item.Changes != nil {
			t.Errorf("item = %+v", item)
		}
	})
	t.Run("a cut worktree list leaves registered null", func(t *testing.T) {
		lab, _ := clean(t)
		spy := &cleanGitSpy{override: func(sub string, _ cleanToolSpec, output cleanToolOutput) cleanToolOutput {
			if sub == "worktree" {
				output.Truncated = true
			}
			return output
		}}
		item := lab.only(lab.scan(spy.run))
		if item.Registered != nil || item.Verdict != cleanVerdictStateUnk || !slices.Contains(item.Reasons, cleanReasonGitOutputTruncated) {
			t.Errorf("item = %+v", item)
		}
	})
	t.Run("worktree list runs once for each repository", func(t *testing.T) {
		lab := newCleanWorktreeLab(t)
		first, second := lab.repo("first"), lab.repo("second")
		lab.add(first, "a", "work-a")
		lab.add(first, "b", "work-b")
		lab.add(first, "c", "work-c")
		lab.add(second, "d", "work-d")
		spy := &cleanGitSpy{}
		section := lab.scan(spy.run)
		if len(section.Items) != 4 {
			t.Fatalf("items = %d", len(section.Items))
		}
		if got := spy.count("worktree"); got != 2 {
			t.Fatalf("worktree list ran %d times, want 2", got)
		}
		for _, item := range section.Items {
			if item.Registered == nil || !*item.Registered {
				t.Errorf("%s is not registered: %+v", item.Path, item)
			}
		}
	})
	t.Run("only read-only git commands run, with the safe environment", func(t *testing.T) {
		lab := newCleanWorktreeLab(t)
		repo := lab.repo("main")
		lab.add(repo, "a", "work-a")
		lab.add(repo, "b", "work-b")
		cleanGitRun(t, repo, "config", "branch.work-a.gk-parent", "main")
		cleanGitRun(t, repo, "update-ref", "refs/remotes/origin/main", "main")
		cleanGitRun(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
		spy := &cleanGitSpy{}
		lab.scan(spy.run)
		allowed := []string{"rev-parse", "diff-index", "diff-files", "ls-files", "worktree", "config", "symbolic-ref", "merge-base", "rev-list", "log", "reflog"}
		if len(spy.calls) == 0 {
			t.Fatal("git did not run")
		}
		for _, call := range spy.calls {
			if call.Name != "git" || !slices.Contains(allowed, cleanGitSub(call)) {
				t.Errorf("unexpected command: %s %v", call.Name, call.Args)
			}
			if len(call.Args) < 2 || call.Args[0] != "-c" || call.Args[1] != "core.fsmonitor=false" {
				t.Errorf("fsmonitor is not disabled: %v", call.Args)
			}
			for _, want := range []string{"GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"} {
				if !slices.Contains(call.Env, want) {
					t.Errorf("%v lacks %s in %v", call.Args, want, call.Env)
				}
			}
			if !strings.HasPrefix(call.Dir, lab.root) {
				t.Errorf("git ran outside the worktree root: %s", call.Dir)
			}
			if sub := cleanGitSub(call); sub == "worktree" && !slices.Equal(call.Args[2:], []string{"worktree", "list", "--porcelain"}) {
				t.Errorf("worktree subcommand is not list: %v", call.Args)
			}
		}
	})
}

func TestCleanWorktreeFindsOnlyWorktreesWithinTheRules(t *testing.T) {
	home := cleanTestHome(t)
	root := filepath.Join(home, ".gk", "worktree")
	gitDir := func(parts ...string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(append([]string{root}, append(parts, ".git")...)...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitDir("depth1")
	gitDir("a", "b", "c", "d", "depth5")
	gitDir("a", "b", "c", "d", "e", "depth6")
	gitDir("found")
	gitDir("found", "nested")
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, "linked", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "linked"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	deps := cleanTestDeps(home, func(context.Context, cleanToolSpec) cleanToolOutput {
		return cleanToolOutput{Err: errors.New("no git in this test")}
	})
	section := cleanWorktreeScan(context.Background(), deps)
	var got []string
	for _, item := range section.Items {
		rel, _ := filepath.Rel(root, item.Path)
		got = append(got, rel)
		if item.Kind != cleanWorktreeRepository || item.Verdict != cleanVerdictStateUnk {
			t.Errorf("item = %+v", item)
		}
	}
	want := []string{"a/b/c/d/depth5", "depth1", "found"}
	if !slices.Equal(got, want) {
		t.Fatalf("found %v, want %v", got, want)
	}
	if section.Truncated {
		t.Error("truncated")
	}
}

func TestCleanWorktreeStopsAtTheLimitAndSaysPartial(t *testing.T) {
	home := cleanTestHome(t)
	root := filepath.Join(home, ".gk", "worktree")
	for index := 0; index < cleanWorktreeLimit+1; index++ {
		if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("w%03d", index), ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	deps := cleanTestDeps(home, func(context.Context, cleanToolSpec) cleanToolOutput {
		return cleanToolOutput{Err: errors.New("no git in this test")}
	})
	section := cleanWorktreeScan(context.Background(), deps)
	if len(section.Items) != cleanWorktreeLimit || !section.Truncated || section.Status != cleanPartial ||
		!slices.Contains(section.Reasons, cleanReasonWorktreeLimit) {
		t.Fatalf("items = %d, truncated = %v, status = %s, reasons = %v", len(section.Items), section.Truncated, section.Status, section.Reasons)
	}
}

func TestCleanWorktreeSectionStates(t *testing.T) {
	home := cleanTestHome(t)
	deps := cleanTestDeps(home, runCleanTool)
	t.Run("a missing root is quiet and has no usage", func(t *testing.T) {
		section := cleanWorktreeScan(context.Background(), deps)
		if section.Status != cleanAbsent || section.UsageBytes != nil || section.Leftover != nil || section.Items == nil || !cleanWorktreesQuiet(section) {
			t.Fatalf("section = %+v", section)
		}
		data, err := json.Marshal(section)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "usage_bytes") || !strings.Contains(string(data), `"items":[]`) {
			t.Fatalf("JSON = %s", data)
		}
		quiet := deps
		quiet.run = func(_ context.Context, spec cleanToolSpec) cleanToolOutput {
			return cleanToolOutput{Stdout: filepath.Join(home, "absent", spec.Name) + "\n"}
		}
		if report := cleanCollect(context.Background(), quiet, "linux"); report.worktrees.Status != cleanAbsent {
			t.Fatalf("worktrees = %+v", report.worktrees)
		}
	})
	root := filepath.Join(home, ".gk", "worktree")
	if err := os.MkdirAll(filepath.Join(root, "w", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Run("a missing git is tool_absent", func(t *testing.T) {
		missing := deps
		missing.lookPath = func(string) (string, error) { return "", errors.New("not found") }
		section := cleanWorktreeScan(context.Background(), missing)
		if section.Status != cleanToolAbsent || len(section.Items) != 0 || cleanWorktreesQuiet(section) {
			t.Fatalf("section = %+v", section)
		}
	})
	t.Run("a stopped scan is not scanned", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		section := cleanWorktreeScan(ctx, deps)
		if section.Status != cleanNotScanned || !slices.Equal(section.Reasons, []string{cleanReasonStopped}) {
			t.Fatalf("section = %+v", section)
		}
	})
	t.Run("a scan that stops midway marks the rest state_unknown", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		stopping := deps
		stopping.run = func(context.Context, cleanToolSpec) cleanToolOutput {
			cancel()
			return cleanToolOutput{Err: errors.New("killed")}
		}
		section := cleanWorktreeScan(ctx, stopping)
		if section.Status != cleanPartial || len(section.Items) != 1 || section.Items[0].Verdict != cleanVerdictStateUnk ||
			!slices.Contains(section.Reasons, cleanReasonStopped) {
			t.Fatalf("section = %+v", section)
		}
	})
}

func TestCleanWorktreeUsageComesFromOneMeasurementOfTheRoot(t *testing.T) {
	home := cleanTestHome(t)
	root := filepath.Join(home, ".gk", "worktree")
	for _, name := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(root, name, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fail := func(context.Context, cleanToolSpec) cleanToolOutput {
		return cleanToolOutput{Err: errors.New("no git")}
	}
	measured := func(partial bool) (cleanWorktrees, int) {
		var calls int
		deps := cleanTestDeps(home, fail)
		deps.measure = func(context.Context, duOptions, duSources) (duScan, error) {
			calls++
			tree := func(name string, total uint64) *duNode { return &duNode{name: name, dir: true, total: total, count: 1} }
			scan := cleanFakeScan(100)
			scan.root.children = []*duNode{tree("a", 40), tree("b", 25), {name: "stray", total: 10}}
			scan.partial = partial
			return scan, nil
		}
		return cleanWorktreeScan(context.Background(), deps), calls
	}
	t.Run("a complete scan", func(t *testing.T) {
		section, calls := measured(false)
		if calls != 1 {
			t.Fatalf("measure ran %d times", calls)
		}
		if section.Status != cleanMeasured || section.UsageBytes == nil || *section.UsageBytes != 100 || section.LowerBound ||
			section.Leftover == nil || section.Leftover.UsageBytes != 35 {
			t.Fatalf("section = %+v", section)
		}
		usage := map[string]uint64{}
		for _, item := range section.Items {
			if item.UsageBytes == nil || item.LowerBound {
				t.Fatalf("item = %+v", item)
			}
			usage[filepath.Base(item.Path)] = *item.UsageBytes
		}
		if usage["a"] != 40 || usage["b"] != 25 {
			t.Fatalf("usage = %v", usage)
		}
	})
	t.Run("a partial scan makes every value a lower bound", func(t *testing.T) {
		section, _ := measured(true)
		if section.Status != cleanPartial || !section.LowerBound || section.Leftover == nil {
			t.Fatalf("section = %+v", section)
		}
		for _, item := range section.Items {
			if !item.LowerBound || item.UsageBytes == nil {
				t.Fatalf("item = %+v", item)
			}
		}
	})
	t.Run("a worktree the tree lacks has no size, not zero", func(t *testing.T) {
		deps := cleanTestDeps(home, fail)
		deps.measure = func(context.Context, duOptions, duSources) (duScan, error) { return cleanFakeScan(100), nil }
		section := cleanWorktreeScan(context.Background(), deps)
		for _, item := range section.Items {
			if item.UsageBytes != nil {
				t.Fatalf("item = %+v", item)
			}
		}
	})
	t.Run("a root that cannot be read still lists the worktrees", func(t *testing.T) {
		deps := cleanTestDeps(home, fail)
		deps.measure = func(context.Context, duOptions, duSources) (duScan, error) {
			return duScan{root: &duNode{dir: true, unreadable: true}}, nil
		}
		section := cleanWorktreeScan(context.Background(), deps)
		if section.Status != cleanPartial || section.UsageBytes != nil || section.Leftover != nil || len(section.Items) != 2 ||
			!slices.Contains(section.Reasons, cleanReasonRootUnreadable) {
			t.Fatalf("section = %+v", section)
		}
	})
}

func TestCleanWorktreeJSONHasNoModificationTime(t *testing.T) {
	lab := newCleanWorktreeLab(t)
	repo := lab.repo("main")
	lab.add(repo, "w", "work")
	data, err := json.Marshal(lab.scan(nil))
	if err != nil {
		t.Fatal(err)
	}
	var decoded interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	var keys []string
	var collect func(value interface{})
	collect = func(value interface{}) {
		switch typed := value.(type) {
		case map[string]interface{}:
			for key, child := range typed {
				keys = append(keys, key)
				collect(child)
			}
		case []interface{}:
			for _, child := range typed {
				collect(child)
			}
		}
	}
	collect(decoded)
	for _, key := range keys {
		if lower := strings.ToLower(key); strings.Contains(lower, "mtime") || strings.Contains(lower, "modif") {
			t.Errorf("JSON has a modification time key %q", key)
		}
	}
	for _, key := range []string{"last_commit_at", "head_moved_at", "registered", "changes", "untracked", "verdict", "base_source", "leftover", "cleanup", "root"} {
		if !slices.Contains(keys, key) {
			t.Errorf("JSON lacks %q", key)
		}
	}
}

func TestCleanWorktreeUsageOfRealFiles(t *testing.T) {
	lab := newCleanWorktreeLab(t)
	repo := lab.repo("main")
	worktree := lab.add(repo, "w", "work")
	lab.write(filepath.Join(worktree, "big"), strings.Repeat("x", 256<<10))
	section := lab.scan(nil)
	item := lab.only(section)
	if item.UsageBytes == nil || *item.UsageBytes < 256<<10 || section.UsageBytes == nil || *section.UsageBytes < *item.UsageBytes ||
		section.Leftover == nil || section.Leftover.UsageBytes != *section.UsageBytes-*item.UsageBytes {
		t.Fatalf("section = %+v", section)
	}
}

func TestCleanWorktreeTextShowsStatesAndCapsTheList(t *testing.T) {
	setLanguage("en")
	t.Cleanup(func() { setLanguage("en") })
	usage := uint64(2 << 20)
	item := func(index int, verdict string) cleanWorktree {
		size := usage + uint64(index)<<10
		return cleanWorktree{Path: fmt.Sprintf("/h/.gk/worktree/repo/w%02d", index), Kind: cleanWorktreeLinked, Branch: fmt.Sprintf("b%02d", index),
			Verdict: verdict, UsageBytes: &size, BaseSource: cleanBaseNone}
	}
	section := cleanWorktrees{Status: cleanMeasured, Root: "/h/.gk/worktree", Cleanup: cleanWorktreeCleanup, Leftover: &cleanWorktreeLeftover{UsageBytes: 1 << 20}}
	for index := 0; index < 12; index++ {
		section.Items = append(section.Items, item(index, cleanVerdictMergedClean))
	}
	changes, untracked := 2, 1
	dirty := item(20, cleanVerdictHasChanges)
	dirty.Changes, dirty.Untracked, dirty.CountsLowerBound = &changes, &untracked, true
	unknown := cleanWorktree{Path: "/h/.gk/worktree/repo/unknown", Kind: cleanWorktreeLinked, Verdict: cleanVerdictStateUnk, Reasons: []string{cleanReasonToolFailed}, Detail: "exit 3"}
	section.Items = append(section.Items, dirty, unknown)

	text := formatCleanWorktrees(section, false, 0)
	for _, want := range []string{"Worktrees under /h/.gk/worktree", "not added to any total", "merged, no changes  12", "has changes  1  2.0 MB",
		"state unknown  1  not reported", "at least 2 changed, 1 untracked", "more, -v shows more", "git-kit worktree cleanup", "other files in the folder: 1.0 MB"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if lines := strings.Count(text, "repo/w") + strings.Count(text, "repo/unknown"); lines != cleanWorktreeShown {
		t.Errorf("listed %d worktrees, want %d:\n%s", lines, cleanWorktreeShown, text)
	}
	// 크기를 모르는 worktree는 0 B가 아니라 상태로 보인다.
	if strings.Contains(text, "0 B") {
		t.Errorf("text shows 0 B:\n%s", text)
	}
	verbose := formatCleanWorktrees(section, true, 0)
	if strings.Count(verbose, "repo/w")+strings.Count(verbose, "repo/unknown") != len(section.Items) || !strings.Contains(verbose, "exit 3") ||
		!strings.Contains(verbose, "tool failed") || strings.Contains(verbose, "more, -v shows more") {
		t.Errorf("verbose text:\n%s", verbose)
	}
	absent := formatCleanWorktrees(cleanWorktrees{Status: cleanAbsent, Root: "/h/.gk/worktree"}, false, 0)
	if !strings.Contains(absent, "worktree folder not found: /h/.gk/worktree") || strings.Contains(absent, "0 B") {
		t.Errorf("absent text:\n%s", absent)
	}
	failed := formatCleanWorktrees(cleanWorktrees{Status: cleanUnreadable, Reasons: []string{cleanReasonPermission}}, false, 0)
	if !strings.Contains(failed, "Worktrees: unreadable (permission denied)") {
		t.Errorf("failed text:\n%s", failed)
	}
}

func TestCleanWorktreeVerdictOrder(t *testing.T) {
	zero, one := 0, 1
	yes, no := true, false
	base := cleanWorktree{Kind: cleanWorktreeLinked, Changes: &zero, Untracked: &zero, Merged: &yes}
	with := func(edit func(*cleanWorktree)) cleanWorktree {
		item := base
		edit(&item)
		return item
	}
	tests := []struct {
		name string
		item cleanWorktree
		want string
	}{
		{"clean", base, cleanVerdictMergedClean},
		{"unlinked beats changes", with(func(i *cleanWorktree) { i.Kind, i.Changes = cleanWorktreeUnlinked, &one }), cleanVerdictUnlinked},
		{"changes beat failure", with(func(i *cleanWorktree) { i.Changes, i.failed = &one, true }), cleanVerdictHasChanges},
		{"failure beats locked", with(func(i *cleanWorktree) { i.failed, i.Locked = true, true }), cleanVerdictStateUnk},
		{"locked beats not merged", with(func(i *cleanWorktree) { i.Locked, i.Merged = true, &no }), cleanVerdictLocked},
		{"not merged beats missing base", with(func(i *cleanWorktree) { i.Merged = &no }), cleanVerdictNotMerged},
		{"missing base", with(func(i *cleanWorktree) { i.Merged = nil }), cleanVerdictBaseUnknown},
		{"unknown counts", with(func(i *cleanWorktree) { i.Changes = nil }), cleanVerdictStateUnk},
	}
	for _, test := range tests {
		if got := cleanWorktreeVerdict(test.item); got != test.want {
			t.Errorf("%s: verdict = %q, want %q", test.name, got, test.want)
		}
	}
}

func TestCleanNotMeasuredNamesWorktreesOutsideTheRoot(t *testing.T) {
	for _, item := range cleanNotMeasured {
		if item.ID == "worktrees" || item.Reason == "unsaved_work" {
			t.Fatalf("not_measured still has %+v", item)
		}
	}
	if !slices.Contains(cleanNotMeasured, cleanNotMeasuredItem{"other_worktrees", "outside_root"}) {
		t.Fatalf("not_measured = %+v", cleanNotMeasured)
	}
}

// 바뀐 파일을 셀 때 저장소가 정한 clean filter(git-lfs 같은)를 실행하지 않는다. git status는 stat이 바뀐 파일을 filter로
// 읽어 비교한다. stat만 비교하므로 touch만 한 파일도 바뀐 것으로 센다(#268).
func TestCleanWorktreeCountsChangesWithoutCleanFilters(t *testing.T) {
	lab := newCleanWorktreeLab(t)
	repo := lab.repo("main")
	marker := filepath.Join(lab.home, "filter-ran")
	cleanGitRun(t, repo, "config", "filter.probe.clean", "sh -c 'echo ran >> "+marker+"; cat'")
	cleanGitRun(t, repo, "config", "filter.probe.smudge", "cat")
	lab.write(filepath.Join(repo, ".gitattributes"), "*.dat filter=probe\n")
	lab.write(filepath.Join(repo, "a.dat"), "data\n")
	cleanGitRun(t, repo, "add", ".gitattributes", "a.dat")
	cleanGitRun(t, repo, "commit", "-q", "-m", "data")
	worktree := lab.add(repo, "w", "work")
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	// stat을 index와 다르게 만든다. 1초 안의 변경은 mtime이 같을 수 있다.
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(worktree, "a.dat"), later, later); err != nil {
		t.Fatal(err)
	}
	item := lab.only(lab.scan((&cleanGitSpy{}).run))
	if data, err := os.ReadFile(marker); err == nil {
		t.Fatalf("the clean filter ran: %q", data)
	}
	if item.Changes == nil || *item.Changes != 1 || item.Untracked == nil || *item.Untracked != 0 || item.Verdict != cleanVerdictHasChanges {
		t.Fatalf("item = %+v", item)
	}
}
