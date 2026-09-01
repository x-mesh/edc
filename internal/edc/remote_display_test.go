package edc

import (
	"strings"
	"testing"
)

func TestRemoteStreamLayout(t *testing.T) {
	var output strings.Builder
	display := newRemoteDisplay(&output, true, false)
	writer := &remoteStreamWriter{display: display, prefix: "jw-server/git-kit/command"}
	_, _ = writer.Write([]byte("first\nsecond"))
	writer.Flush()
	text := output.String()
	if !strings.Contains(text, "jw-server/git-kit/command") || !strings.Contains(text, "| first") || !strings.Contains(text, "| second") {
		t.Fatalf("layout = %q", text)
	}
}
