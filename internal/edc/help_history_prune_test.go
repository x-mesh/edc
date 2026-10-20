package edc

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestHistoryPruneHelpGroupsRelevantContentAndFitsWidth(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range []string{"en", "ko", "ja"} {
		setLanguage(language)
		for _, width := range []int{40, 64, 80, 88} {
			var plain, colored strings.Builder
			renderHistoryPruneHelp(&plain, width, false)
			renderHistoryPruneHelp(&colored, width, true)
			plainBody := plain.String()[strings.Index(plain.String(), "\n\n")+2:]
			stripped := ansi.Strip(colored.String())
			coloredBody := stripped[strings.Index(stripped, "\n\n")+2:]
			coloredBody = strings.ReplaceAll(coloredBody, "│ ", "  ")
			var bodyLines []string
			for _, line := range strings.Split(coloredBody, "\n") {
				if strings.HasPrefix(line, "╭─ ") {
					bodyLines = append(bodyLines, strings.TrimSuffix(strings.TrimPrefix(line, "╭─ "), " ───"))
				} else if line != "╰─" {
					bodyLines = append(bodyLines, line)
				}
			}
			if strings.Join(bodyLines, "\n") != plainBody {
				t.Fatalf("plain/color help differs at %s/%d", language, width)
			}
			if strings.Contains(plain.String(), "`") || strings.Contains(plain.String(), "\x1b[") {
				t.Fatal("plain help contains formatting markup")
			}
			for _, line := range strings.Split(colored.String(), "\n") {
				if liveWidth(line) > width {
					t.Fatalf("help line exceeds %s/%d: %q", language, width, line)
				}
			}
			for _, key := range []string{"help.usage_label", "help.options_label", "history.prune_help.interactive_title", "history.prune_help.deletion_title", "history.prune_help.storage_title"} {
				if !strings.Contains(plain.String(), T(key)) {
					t.Fatalf("missing help section %s", key)
				}
			}
			for _, unrelated := range []string{"--probe", "--metric", "--process", "PSI"} {
				if strings.Contains(plain.String(), unrelated) {
					t.Fatalf("prune help contains unrelated option %s", unrelated)
				}
			}
		}
	}
}

func TestHistoryPruneHelpDistinguishesSyntaxRoles(t *testing.T) {
	text := "edc history prune --older-than <age> [<DB>] 30d"
	colored := helpSyntax(text, true)
	for _, part := range []string{historyStrong("<age>", richYellow, true), historyStrong("<DB>", richYellow, true), historyStrong("30d", richGreen, true)} {
		if !strings.Contains(colored, part) {
			t.Fatalf("missing syntax color %q: %q", part, colored)
		}
	}
	if ansi.Strip(colored) != text || helpSyntax(text, false) != text {
		t.Fatal("syntax highlighting changed the command")
	}
}

func TestHistoryPruneHelpDoesNotRequireOrCreateDatabase(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	for _, args := range [][]string{{"--help"}, {"--older-than", "7d", "--help"}, {"-h"}} {
		code, stdout, stderr := runHistoryPruneTest(args...)
		if code != 0 || stderr != "" || !strings.Contains(stdout, T("history.prune_help.interactive_title")) || !strings.Contains(stdout, "<DB>") {
			t.Fatalf("help failed for %v: %d %q %q", args, code, stdout, stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "edc")); !os.IsNotExist(err) {
		t.Fatal("help created a history directory")
	}
}

func TestHistoryPruneHelpRoutesThroughApp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, args := range [][]string{{"history", "prune", "--help"}, {"history", "prune", "--older-than", "30d", "--help"}, {"help", "history", "prune"}} {
		file, err := os.CreateTemp(t.TempDir(), "help")
		if err != nil {
			t.Fatal(err)
		}
		previous := os.Stdout
		var code int
		func() {
			os.Stdout = file
			defer func() { os.Stdout = previous }()
			code = Run(args, "test")
		}()
		if _, err := file.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(file)
		file.Close()
		if err != nil || code != 0 || strings.Contains(string(data), "--probe") || !strings.Contains(string(data), "<DB>") {
			t.Fatalf("app did not use prune help: %v code=%d err=%v output=%s", args, code, err, data)
		}
	}
}
