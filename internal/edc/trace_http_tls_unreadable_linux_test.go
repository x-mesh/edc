//go:build linux

package edc

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// BPF는 page fault 없이 user memory를 읽으므로, 읽을 수 없는 평문은 잃은 event로 세야 한다. 실제 host에서는 kernel이 page를
// 바꾸는 드문 순간에만 생기므로, 시험은 PROT_NONE page를 buffer로 넘겨 같은 실패를 만든다.
func TestTraceTLSCountsUnreadablePlaintext(t *testing.T) {
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("TLS plaintext fixture needs BPF and perf capabilities")
	}
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("TLS plaintext fixture needs a C compiler")
	}
	source := `
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>
__attribute__((noinline)) int SSL_write(void *ssl, const void *buf, int num) {
 __asm__ volatile("" : : "r"(ssl), "r"(buf) : "memory");
 return num;
}
int main(void) {
 long page = sysconf(_SC_PAGESIZE);
 char *pages = mmap(NULL, 2 * page, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
 if (pages == MAP_FAILED || mprotect(pages + page, page, PROT_NONE) != 0) return 1;
 int conn = 0;
 puts("ready"); fflush(stdout); sleep(3);
 char request[] = "GET /readable HTTP/1.1\r\nHost: localhost\r\n\r\n";
 SSL_write(&conn, request, sizeof(request) - 1);
 SSL_write(&conn, pages + page, 64);
 char *tail = pages + page - 16;
 memcpy(tail, "GET /tail HTTP/1", 16);
 SSL_write(&conn, tail, 256);
 puts("done");
 return 0;
}
`
	fixture := filepath.Join(t.TempDir(), "tls-unreadable")
	build := exec.Command(compiler, "-x", "c", "-", "-O0", "-o", fixture)
	build.Stdin = strings.NewReader(source)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("TLS plaintext fixture: %v\n%s", err, output)
	}
	finder, _, _, err := resolveTraceTLSTargets(traceTLSMode(fixture))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, fixture)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	if line, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("TLS plaintext ready: %q, %v", line, err)
	}
	var paths []string
	summary, err := collectTraceEventsLive(traceScope{protocol: "http", tls: traceTLSMode(fixture), tlsFinder: finder}, 4*time.Second, func(event captureEvent) error {
		if event.PID == uint32(command.Process.Pid) && event.TLS && event.Event == "http_request" {
			paths = append(paths, event.Path)
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	output, readErr := io.ReadAll(reader)
	if err := command.Wait(); err != nil || readErr != nil || !strings.Contains(string(output), "done") {
		t.Fatalf("TLS plaintext fixture: %v, %v, %s", err, readErr, output)
	}
	if len(paths) != 1 || paths[0] != "/readable" {
		t.Fatalf("readable requests = %q", paths)
	}
	// 앞을 읽지 못한 호출과, 앞은 읽었지만 뒤를 읽지 못한 호출이 하나씩이다. 같은 host의 다른 traffic이 더할 수 있어 하한만 본다.
	if summary.LostEvents < 2 {
		t.Fatalf("lost events = %d, want at least 2", summary.LostEvents)
	}
}
