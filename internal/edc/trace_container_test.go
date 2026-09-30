package edc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func fakeTraceContainer(t *testing.T, inspect string, cgroup string) func(...string) uint64 {
	t.Helper()
	root := t.TempDir()
	proc, cgroups := filepath.Join(root, "proc"), filepath.Join(root, "cgroup")
	for _, directory := range []string{filepath.Join(proc, "123"), filepath.Join(cgroups, "system.slice", "docker-abc.scope", "inner")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, text := range map[string]string{filepath.Join(proc, "123", "cgroup"): cgroup, filepath.Join(cgroups, "cgroup.controllers"): "cpu memory\n"} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldInspect, oldProc, oldRoots := traceDockerInspect, traceProcRoot, traceCgroupRoots
	t.Cleanup(func() { traceDockerInspect, traceProcRoot, traceCgroupRoots = oldInspect, oldProc, oldRoots })
	traceDockerInspect = func(string) ([]byte, error) { return []byte(inspect), nil }
	traceProcRoot, traceCgroupRoots = proc, []string{filepath.Join(root, "missing"), cgroups}
	inode := func(parts ...string) uint64 {
		info, err := os.Stat(filepath.Join(append([]string{cgroups}, parts...)...))
		if err != nil {
			t.Fatal(err)
		}
		return uint64(info.Sys().(*syscall.Stat_t).Ino)
	}
	return inode
}

func TestResolveTraceContainerCollectsItsCgroups(t *testing.T) {
	inode := fakeTraceContainer(t, "123 /web\n", "0::/system.slice/docker-abc.scope\n")
	container, code, err := resolveTraceContainer("web")
	if err != nil || code != 0 {
		t.Fatalf("resolve: code %d, %v", code, err)
	}
	scope, inner, parent := inode("system.slice", "docker-abc.scope"), inode("system.slice", "docker-abc.scope", "inner"), inode("system.slice")
	if container.name != "web" || len(container.cgroups) != 2 || !container.cgroups[scope] || !container.cgroups[inner] || container.cgroups[parent] {
		t.Fatalf("container = %+v, want scope %d and inner %d", container, scope, inner)
	}

	options := tcpTraceOptions{process: "nginx", container: container}
	for _, test := range []struct {
		event captureEvent
		want  bool
	}{
		{captureEvent{Process: "nginx", CgroupID: scope}, true},
		{captureEvent{Process: "nginx", CgroupID: inner}, true},
		{captureEvent{Process: "nginx", CgroupID: parent}, false},
		// 주인을 모르는 socket의 event는 cgroup ID가 0이다.
		{captureEvent{Process: "nginx"}, false},
		{captureEvent{Process: "curl", CgroupID: scope}, false},
	} {
		if got := options.matches(test.event); got != test.want {
			t.Fatalf("matches(%+v) = %v, want %v", test.event, got, test.want)
		}
	}
	if !(tcpTraceOptions{}).matches(captureEvent{}) {
		t.Fatal("without filters every event must match")
	}
}

func TestResolveTraceContainerFailures(t *testing.T) {
	for _, test := range []struct {
		name, inspect, cgroup string
		code                  int
		message               string
	}{
		{"stopped", "0 /web", "0::/system.slice/docker-abc.scope\n", 2, "container web is not running"},
		{"cgroup v1", "123 /web", "12:pids:/docker/abc\n", 3, "cgroup v1 is not supported"},
		{"root cgroup", "123 /web", "0::/\n", 3, "root cgroup"},
		{"other mount", "123 /web", "0::/kubepods/abc\n", 3, "is not under"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fakeTraceContainer(t, test.inspect, test.cgroup)
			_, code, err := resolveTraceContainer("web")
			if code != test.code || err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("code %d, err %v, want %d and %q", code, err, test.code, test.message)
			}
		})
	}

	fakeTraceContainer(t, "", "")
	traceDockerInspect = func(string) ([]byte, error) {
		return exec.Command("sh", "-c", "echo 'Error: No such container: nope' >&2; exit 1").Output()
	}
	if _, code, err := resolveTraceContainer("nope"); code != 2 || err == nil || !strings.Contains(err.Error(), "No such container: nope") {
		t.Fatalf("missing container: code %d, err %v", code, err)
	}
	traceDockerInspect = func(string) ([]byte, error) {
		return exec.Command("edc-no-such-docker-command").Output()
	}
	if _, code, err := resolveTraceContainer("web"); code != 3 || err == nil || !strings.Contains(err.Error(), "docker command") {
		t.Fatalf("no docker: code %d, err %v", code, err)
	}
}

func TestTraceContainerOptionNeedsProcessEvents(t *testing.T) {
	for _, protocol := range []string{"arp", "ndp"} {
		if code := runTrace([]string{protocol, "--container", "web"}); code != 2 {
			t.Fatalf("trace %s --container exit = %d, want 2", protocol, code)
		}
	}
}
