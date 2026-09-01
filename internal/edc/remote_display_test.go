package edc

import (
	"strings"
	"testing"
)

func TestRemoteStreamLayout(t *testing.T) {
	var output strings.Builder
	display := newRemoteDisplay(&output, remoteDisplayOptions{verbose: true})
	writer := &remoteStreamWriter{display: display, prefix: "jw-server/git-kit/command"}
	_, _ = writer.Write([]byte("first\nsecond"))
	writer.Flush()
	text := output.String()
	if !strings.Contains(text, "jw-server/git-kit/command") || !strings.Contains(text, "| first") || !strings.Contains(text, "| second") {
		t.Fatalf("layout = %q", text)
	}
}

func TestRemoteVerboseStatusLineStaysLast(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var output strings.Builder
	display := newRemoteDisplay(&output, remoteDisplayOptions{verbose: true, spinner: true})
	display.Start("jw-server", "git-kit", "command")
	display.WriteLine("jw-server/git-kit/command", "updating")
	display.Close()
	text := output.String()
	lineIndex := strings.Index(text, "updating")
	statusIndex := strings.LastIndex(text, "running  jw-server / git-kit / command")
	if lineIndex < 0 || statusIndex < lineIndex {
		t.Fatalf("status line is not after streamed output: %q", text)
	}
	if !strings.HasSuffix(text, "\r\033[2K") {
		t.Fatalf("status line was not cleared: %q", text)
	}
	if !strings.Contains(text, "\033[97m⠋  running") {
		t.Fatalf("status line is not white: %q", text)
	}
}
