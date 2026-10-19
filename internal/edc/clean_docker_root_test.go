package edc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const cleanTestSecret = "DB_PASSWORD=hunter2-do-not-print"

func cleanTestWriteFile(t *testing.T, path, content string, modified time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

// cleanTestStaleRoot는 엔진이 옮겨 가서 남은 옛 데이터 디렉터리를 만든다.
func cleanTestStaleRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	anonymous := strings.Repeat("ab", 32)
	buildx := time.Date(2026, 3, 27, 10, 0, 0, 0, time.UTC)
	redis := time.Date(2025, 9, 4, 8, 0, 0, 0, time.UTC)
	cleanTestWriteFile(t, filepath.Join(root, "volumes", "buildx_state", "_data", "cache.db"), strings.Repeat("x", 100000), buildx)
	cleanTestWriteFile(t, filepath.Join(root, "volumes", "llm-bot_redis_data", "_data", "dump.rdb"), "redis", redis)
	cleanTestWriteFile(t, filepath.Join(root, "volumes", anonymous, "_data", "a"), "a", redis)
	cleanTestWriteFile(t, filepath.Join(root, "volumes", "metadata.db"), "bolt", redis)
	cleanTestWriteFile(t, filepath.Join(root, "overlay2", "layer", "diff", "big"), strings.Repeat("y", 50000), redis)
	cleanTestWriteFile(t, filepath.Join(root, "containers", "0123456789abcdef", "config.v2.json"), fmt.Sprintf(`{
		"Name": "/buildx_builder", "Created": "2025-09-30T01:02:03.5Z",
		"Config": {"Image": "moby/buildkit:stable", "Env": [%q], "Labels": {"com.docker.compose.project": "devbox"}},
		"State": {"Running": false, "FinishedAt": "2026-04-29T00:00:00Z"},
		"MountPoints": {"/var/lib/buildkit": {"Type": "volume", "Name": "buildx_state"}, "/etc": {"Type": "bind", "Name": ""}}
	}`, cleanTestSecret), redis)
	cleanTestWriteFile(t, filepath.Join(root, "containers", "fedcba9876543210", "config.v2.json"), `{
		"Name": "/never-started", "Created": "2026-02-01T00:00:00Z", "Config": {"Image": "app"},
		"State": {"Running": false, "FinishedAt": "0001-01-01T00:00:00Z"}
	}`, redis)
	cleanTestWriteFile(t, filepath.Join(root, "containers", "badbadbadbad", "config.v2.json"), "{not json", redis)
	return root
}

func cleanTestRootDeps(t *testing.T) cleanDeps {
	t.Helper()
	host, ok := hostDuSources()
	if !ok {
		t.Skip("du is not supported on this platform")
	}
	deps := cleanTestDeps(cleanTestHome(t), nil)
	deps.measure, deps.sources = duMeasure, cleanSources(host)
	return deps
}

func cleanTestProc(t *testing.T, targets map[string][]string) string {
	t.Helper()
	proc := t.TempDir()
	for pid, links := range targets {
		fdDir := filepath.Join(proc, pid, "fd")
		if err := os.MkdirAll(fdDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for index, target := range links {
			if err := os.Symlink(target, filepath.Join(fdDir, fmt.Sprint(index))); err != nil {
				t.Fatal(err)
			}
		}
	}
	return proc
}

func cleanTestUseProc(t *testing.T, dir string) {
	t.Helper()
	old := cleanProcDir
	cleanProcDir = dir
	t.Cleanup(func() { cleanProcDir = old })
}

func TestCleanDockerRootExplainsWhereEachVolumeCameFrom(t *testing.T) {
	stale := cleanTestStaleRoot(t)
	current := t.TempDir()
	cleanTestUseProc(t, cleanTestProc(t, map[string][]string{"100": {"/dev/null", "/somewhere/else"}, "self": {stale + "/ignored"}}))
	live := cleanDockerLive{known: true, volumes: map[string]bool{"llm-bot_redis_data": true}, containers: map[string]string{"web": "devbox"}}
	docker := cleanDocker{Status: cleanMeasured, RootDir: current, live: live}

	section := cleanDockerRootScan(context.Background(), cleanTestRootDeps(t), "linux", docker, stale)
	if section.Status != cleanMeasured || section.Current != current || len(section.Roots) != 1 {
		t.Fatalf("section = %+v", section)
	}
	root := section.Roots[0]
	if root.Status != cleanMeasured || root.UsageBytes == nil || *root.UsageBytes < 150000 {
		t.Fatalf("root = %+v", root)
	}
	if root.Cleanup != "rm -rf --one-file-system "+cleanShellQuote(root.Path) {
		t.Fatalf("cleanup = %q", root.Cleanup)
	}
	if strings.Contains(root.Cleanup, "docker volume") {
		t.Fatalf("a per-volume docker command would remove the volume of the running engine: %q", root.Cleanup)
	}
	if root.OpenFiles == nil || *root.OpenFiles != 0 || !root.OpenFilesComplete {
		t.Fatalf("open files = %v complete %v", root.OpenFiles, root.OpenFilesComplete)
	}
	if root.LastContainerStop == nil || cleanDockerDate(root.LastContainerStop) != "2026-04-29" {
		t.Fatalf("last stop = %v", root.LastContainerStop)
	}
	if root.NewestVolumeWrite == nil || cleanDockerDate(root.NewestVolumeWrite) != "2026-03-27" {
		t.Fatalf("newest write = %v", root.NewestVolumeWrite)
	}
	if root.VolumesTotal != 3 || len(root.Volumes) != 3 || root.Volumes[0].Name != "buildx_state" {
		t.Fatalf("volumes = %+v", root.Volumes)
	}
	buildx := root.Volumes[0]
	if len(buildx.UsedBy) != 1 || buildx.UsedBy[0] != "buildx_builder" || buildx.Anonymous || buildx.InEngine == nil || *buildx.InEngine {
		t.Fatalf("buildx volume = %+v", buildx)
	}
	var redis, anonymous cleanOldVolume
	for _, volume := range root.Volumes {
		switch {
		case volume.Name == "llm-bot_redis_data":
			redis = volume
		case volume.Anonymous:
			anonymous = volume
		}
	}
	if len(redis.UsedBy) != 0 || redis.InEngine == nil || !*redis.InEngine || cleanDockerDate(redis.NewestWrite) != "2025-09-04" {
		t.Fatalf("redis volume = %+v", redis)
	}
	if anonymous.Name == "" {
		t.Fatalf("anonymous volume missing: %+v", root.Volumes)
	}
	if len(root.Containers) != 2 || root.ContainersUnreadable != 1 {
		t.Fatalf("containers = %+v unreadable %d", root.Containers, root.ContainersUnreadable)
	}
	builder := root.Containers[0]
	if builder.Name != "buildx_builder" || builder.ID != "0123456789ab" || builder.Image != "moby/buildkit:stable" || builder.Project != "devbox" ||
		builder.Running || builder.ProjectInEngine == nil || !*builder.ProjectInEngine || builder.InEngine == nil || *builder.InEngine {
		t.Fatalf("builder = %+v", builder)
	}
	if never := root.Containers[1]; never.Finished != nil || never.Created == nil {
		t.Fatalf("never started = %+v", never)
	}
	data, err := json.Marshal(section)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hunter2") {
		t.Fatalf("report leaks container environment: %s", data)
	}
}

func TestCleanDockerRootCountsOpenFilesAndFlagsUnreadableProcesses(t *testing.T) {
	stale := cleanTestStaleRoot(t)
	cleanTestUseProc(t, cleanTestProc(t, map[string][]string{"7": {stale + "/volumes/metadata.db", stale + "-sibling/file", "/dev/null"}}))
	count, complete, readable := cleanOpenUnder(context.Background(), cleanProcDir, stale)
	if count != 1 || !complete || !readable {
		t.Fatalf("count %d complete %v readable %v", count, complete, readable)
	}
	if _, _, readable := cleanOpenUnder(context.Background(), filepath.Join(t.TempDir(), "missing"), stale); readable {
		t.Fatal("missing /proc reported readable")
	}
	unreadable := cleanTestProc(t, map[string][]string{"9": nil})
	if err := os.Chmod(filepath.Join(unreadable, "9", "fd"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(unreadable, "9", "fd"), 0o755) })
	if os.Geteuid() != 0 {
		if _, complete, _ := cleanOpenUnder(context.Background(), unreadable, stale); complete {
			t.Fatal("an unreadable process left the result marked complete")
		}
	}
}

func TestCleanDockerRootOffersNoCleanupWhileTheDirectoryIsInUse(t *testing.T) {
	stale := cleanTestStaleRoot(t)
	cleanTestUseProc(t, cleanTestProc(t, map[string][]string{"7": {cleanResolve(stale) + "/volumes/metadata.db"}}))
	section := cleanDockerRootScan(context.Background(), cleanTestRootDeps(t), "linux", cleanDocker{Status: cleanMeasured, RootDir: "/live"}, stale)
	root := section.Roots[0]
	if root.OpenFiles == nil || *root.OpenFiles != 1 || root.Cleanup != "" {
		t.Fatalf("open files %v cleanup %q", root.OpenFiles, root.Cleanup)
	}
}

func TestCleanShellQuote(t *testing.T) {
	for text, want := range map[string]string{
		"/var/lib/docker": "/var/lib/docker",
		"/data/my docker": "'/data/my docker'",
		"/data/it's":      `'/data/it'\''s'`,
		"":                "''",
	} {
		if got := cleanShellQuote(text); got != want {
			t.Errorf("cleanShellQuote(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestCleanDockerRootStaysQuietWhenThereIsNothingToCompare(t *testing.T) {
	stale := cleanTestStaleRoot(t)
	deps := cleanTestRootDeps(t)
	cleanTestUseProc(t, t.TempDir())
	ctx := context.Background()
	if got := cleanDockerRootScan(ctx, deps, "darwin", cleanDocker{Status: cleanMeasured, RootDir: "/x"}, stale); got.Status != "" {
		t.Fatalf("darwin = %+v", got)
	}
	for _, status := range []string{cleanToolAbsent, cleanNotRunning} {
		if got := cleanDockerRootScan(ctx, deps, "linux", cleanDocker{Status: status}, stale); got.Status != "" {
			t.Fatalf("%s = %+v", status, got)
		}
	}
	unknown := cleanDockerRootScan(ctx, deps, "linux", cleanDocker{Status: cleanUnreachable}, stale)
	if unknown.Status != cleanUnknownLocation || len(unknown.Reasons) != 1 || unknown.Reasons[0] != cleanReasonEngineRootUnknown {
		t.Fatalf("unreachable = %+v", unknown)
	}
	if same := cleanDockerRootScan(ctx, deps, "linux", cleanDocker{Status: cleanMeasured, RootDir: stale}, stale); same.Status != cleanAbsent || len(same.Roots) != 0 {
		t.Fatalf("live root reported as old: %+v", same)
	}
	if missing := cleanDockerRootScan(ctx, deps, "linux", cleanDocker{Status: cleanMeasured, RootDir: "/x"}, filepath.Join(t.TempDir(), "none")); missing.Status != cleanAbsent {
		t.Fatalf("missing = %+v", missing)
	}
	if unrelated := cleanDockerRootScan(ctx, deps, "linux", cleanDocker{Status: cleanMeasured, RootDir: "/x"}, t.TempDir()); unrelated.Status != cleanAbsent {
		t.Fatalf("a directory that is not a docker root = %+v", unrelated)
	}
}

func TestCleanDockerRootTreatsALinkToTheLiveRootAsLive(t *testing.T) {
	live := cleanTestStaleRoot(t)
	link := filepath.Join(t.TempDir(), "docker")
	if err := os.Symlink(live, link); err != nil {
		t.Fatal(err)
	}
	cleanTestUseProc(t, t.TempDir())
	got := cleanDockerRootScan(context.Background(), cleanTestRootDeps(t), "linux", cleanDocker{Status: cleanMeasured, RootDir: live}, link)
	if got.Status != cleanAbsent {
		t.Fatalf("got %+v", got)
	}
}

func TestCleanDockerRootLimitsTheVolumesItDescribes(t *testing.T) {
	root := t.TempDir()
	for index := 0; index < cleanDockerRootVolumeLimit+3; index++ {
		cleanTestWriteFile(t, filepath.Join(root, "volumes", fmt.Sprintf("v%02d", index), "_data", "f"), strings.Repeat("z", 1000*(index+1)), time.Now())
	}
	cleanTestUseProc(t, t.TempDir())
	got := cleanDockerRootScan(context.Background(), cleanTestRootDeps(t), "linux", cleanDocker{Status: cleanMeasured, RootDir: "/live"}, root)
	stale := got.Roots[0]
	if stale.VolumesTotal != cleanDockerRootVolumeLimit+3 || len(stale.Volumes) != cleanDockerRootVolumeLimit || stale.VolumesOmitted != 3 || stale.OmittedBytes == 0 {
		t.Fatalf("total %d listed %d omitted %d bytes %d", stale.VolumesTotal, len(stale.Volumes), stale.VolumesOmitted, stale.OmittedBytes)
	}
	if stale.Volumes[0].Bytes < stale.Volumes[1].Bytes {
		t.Fatalf("volumes are not sorted by size: %+v", stale.Volumes[:2])
	}
}

func TestCleanDockerReadsTheEngineRootAndLiveNames(t *testing.T) {
	fake := newCleanDockerFake()
	fake.body[cleanDockerInfoPath] = `{"OperatingSystem":"Ubuntu","ServerVersion":"29.1","DockerRootDir":"/app/docker-data"}`
	fake.body[cleanDockerAPIPath+"/volumes"] = `{"Volumes":[{"Name":"data"}],"Warnings":null}`
	fake.body[cleanDockerAPIPath+"/containers/json?all=1"] = `[{"Names":["/web-1"],"Labels":{"com.docker.compose.project":"shop"}},{"Names":["/solo"],"Labels":null}]`
	socket := cleanDockerServe(t, fake)
	var dials atomic.Int32
	section := cleanDockerScan(context.Background(), cleanDockerDeps(t, socket, &dials))
	if section.RootDir != "/app/docker-data" || !section.live.known || !section.live.volumes["data"] ||
		section.live.containers["web-1"] != "shop" || section.live.containers["solo"] != "" {
		t.Fatalf("section = %+v live = %+v", section, section.live)
	}
	for _, request := range fake.seen() {
		if !strings.HasPrefix(request, "GET ") {
			t.Fatalf("non-GET request %q", request)
		}
	}
}

func TestCleanDockerLiveStaysUnknownWhenTheEngineRefusesTheList(t *testing.T) {
	fake := newCleanDockerFake()
	fake.status[cleanDockerAPIPath+"/volumes"] = 500
	socket := cleanDockerServe(t, fake)
	var dials atomic.Int32
	section := cleanDockerScan(context.Background(), cleanDockerDeps(t, socket, &dials))
	if section.Status != cleanMeasured || section.live.known {
		t.Fatalf("section = %+v", section)
	}
}

func TestFormatCleanDockerRootShowsTheEvidenceForADecision(t *testing.T) {
	stale := cleanTestStaleRoot(t)
	cleanTestUseProc(t, t.TempDir())
	live := cleanDockerLive{known: true, volumes: map[string]bool{"llm-bot_redis_data": true}, containers: map[string]string{"x": "devbox"}}
	section := cleanDockerRootScan(context.Background(), cleanTestRootDeps(t), "linux", cleanDocker{Status: cleanMeasured, RootDir: "/app/docker-data", live: live}, stale)
	plain := formatCleanDockerRoot(section, false, 0, cleanStyle(false))
	for _, want := range []string{"Old Docker data directory " + cleanResolve(stale), "the running engine uses /app/docker-data", "no open files",
		"last container stop 2026-04-29", "newest volume write 2026-03-27", "buildx_state", "used by buildx_builder",
		"no container record", "a volume with this name exists in the running engine", "1 unnamed volumes", "containers recorded: 2", "(-v lists them)"} {
		if !strings.Contains(plain, want) {
			t.Errorf("output lacks %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "rm -rf") {
		t.Errorf("cleanup shown without -v:\n%s", plain)
	}
	if !strings.Contains(plain, "docker volume rm removes that one, not this one") {
		t.Errorf("no warning about docker volume rm:\n%s", plain)
	}
	if strings.Contains(plain, "moby/buildkit") {
		t.Errorf("container list shown without -v:\n%s", plain)
	}
	verbose := formatCleanDockerRoot(section, true, 0, cleanStyle(false))
	for _, want := range []string{"to clean, run rm -rf --one-file-system " + cleanResolve(stale) + " (edc does not run it)", "moby/buildkit:stable", "project devbox", "stopped 2026-04-29", "the project runs in the running engine", strings.Repeat("ab", 32)} {
		if !strings.Contains(verbose, want) {
			t.Errorf("verbose output lacks %q:\n%s", want, verbose)
		}
	}
	if strings.Contains(plain, "hunter2") || strings.Contains(verbose, "hunter2") {
		t.Error("output leaks container environment")
	}
	if got := formatCleanDockerRoot(cleanDockerRoots{Status: cleanAbsent}, true, 0, cleanStyle(false)); got != "" {
		t.Errorf("absent section printed %q", got)
	}
	unknown := formatCleanDockerRoot(cleanDockerRoots{Status: cleanUnknownLocation, Reasons: []string{cleanReasonEngineRootUnknown}}, false, 0, cleanStyle(false))
	if !strings.Contains(unknown, "the engine did not report its data directory") {
		t.Errorf("unknown output = %q", unknown)
	}
}

func TestCleanDockerReadsTheRootOfAnEngineOlderThanTheTotalsNeed(t *testing.T) {
	fake := newCleanDockerFake()
	fake.api = "1.49"
	fake.body["/v1.49/info"] = `{"OperatingSystem":"Ubuntu","ServerVersion":"28.0","DockerRootDir":"/app/docker-data"}`
	fake.body["/v1.49/volumes"] = `{"Volumes":[{"Name":"data"}]}`
	fake.body["/v1.49/containers/json?all=1"] = `[{"Names":["/web-1"],"Labels":{"com.docker.compose.project":"shop"}}]`
	socket := cleanDockerServe(t, fake)
	var dials atomic.Int32
	section := cleanDockerScan(context.Background(), cleanDockerDeps(t, socket, &dials))
	if section.Status != cleanUnsupported || len(section.Reasons) != 1 || section.Reasons[0] != cleanReasonAPIVersion || len(section.Types) != 0 {
		t.Fatalf("section = %+v", section)
	}
	if section.RootDir != "/app/docker-data" || !section.live.known || !section.live.volumes["data"] || section.live.containers["web-1"] != "shop" {
		t.Fatalf("root %q live %+v", section.RootDir, section.live)
	}
	for _, request := range fake.seen() {
		if strings.Contains(request, "system/df") || !strings.HasPrefix(request, "GET ") {
			t.Fatalf("unexpected request %q", request)
		}
	}
}
