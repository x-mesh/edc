package edc

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeTunnelRunner struct {
	alive   map[string]bool
	started []tunnelPlan
	stopped []string
	failure error
	// failures는 Start가 차례로 돌려줄 오류다. 비면 failure를 따른다.
	failures []error
	attempts int
	// stopFailure는 Stop이 돌려줄 오류다. 오류일 때는 master를 끝내지 않는다.
	stopFailure error
	// probe는 Probe가 받은 ssh 인자마다 출력과 오류를 정한다. 비면 모두 성공한다.
	probe  func(args []string) (string, error)
	probes [][]string
}

func (runner *fakeTunnelRunner) Probe(_ context.Context, args []string) (string, error) {
	runner.probes = append(runner.probes, args)
	if runner.probe == nil {
		return "", nil
	}
	return runner.probe(args)
}

func (runner *fakeTunnelRunner) Start(_ context.Context, plan tunnelPlan, socket string) error {
	runner.attempts++
	if len(runner.failures) > 0 {
		failure := runner.failures[0]
		runner.failures = runner.failures[1:]
		if failure != nil {
			return failure
		}
	} else if runner.failure != nil {
		return runner.failure
	}
	runner.started = append(runner.started, plan)
	runner.alive[socket] = true
	return nil
}

func (runner *fakeTunnelRunner) Check(_ context.Context, _, socket string) (int, error) {
	if !runner.alive[socket] {
		return 0, errors.New("no master")
	}
	return 4242, nil
}

func (runner *fakeTunnelRunner) Stop(_ context.Context, _, socket string) error {
	if runner.stopFailure != nil {
		return runner.stopFailure
	}
	runner.stopped = append(runner.stopped, socket)
	delete(runner.alive, socket)
	return nil
}

// macOS의 t.TempDir()는 unix socket 경로 한도를 넘기므로 /tmp 아래의 짧은 경로를 쓴다.
func shortTunnelDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "edct")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newTestTunnelManager(t *testing.T) (tunnelManager, *fakeTunnelRunner) {
	t.Helper()
	runner := &fakeTunnelRunner{alive: map[string]bool{}}
	manager := newTunnelManager(tunnelStore{dir: shortTunnelDir(t)}, runner, map[string]string{"jump1": "ops@10.0.0.5"})
	manager.pick = func() (int, error) { return 40001, nil }
	manager.available = func(int) error { return nil }
	manager.probe = func(int) string { return tunnelReachOK }
	manager.dial = func(string) error { return nil }
	manager.now = func() time.Time { return time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC) }
	// 실제 expire는 실행 파일을 다시 띄운다. test에서는 그 실행 파일이 test binary라서 띄우지 않는다.
	manager.expire = func(tunnelStore, tunnelRecord) error { return nil }
	return manager, runner
}

func TestBuildTunnelPlanMapsEachKindToSSHForwardArguments(t *testing.T) {
	targets := map[string]string{"jump1": "ops@10.0.0.5"}
	pick := func() (int, error) { return 40001, nil }
	cases := []struct {
		name    string
		spec    tunnelSpec
		forward []string
		port    int
	}{
		{"db", tunnelSpec{Host: "jump1", Local: "5432", Remote: "db.internal:5432"}, []string{"-L", "127.0.0.1:5432:db.internal:5432"}, 5432},
		{"auto", tunnelSpec{Host: "jump1", Local: "auto", Remote: "db:5432"}, []string{"-L", "127.0.0.1:40001:db:5432"}, 40001},
		{"api", tunnelSpec{Host: "dev", Reverse: "8080:localhost:3000"}, []string{"-R", "8080:localhost:3000"}, 8080},
		{"proxy", tunnelSpec{Host: "jump1", Socks: "1080"}, []string{"-D", "127.0.0.1:1080"}, 1080},
	}
	for _, test := range cases {
		plan, err := buildTunnelPlan(test.name, test.spec, targets, pick)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if strings.Join(plan.Forward, " ") != strings.Join(test.forward, " ") || plan.Port != test.port {
			t.Errorf("%s: forward %v port %d, want %v port %d", test.name, plan.Forward, plan.Port, test.forward, test.port)
		}
	}
	plan, _ := buildTunnelPlan("db", cases[0].spec, targets, pick)
	if plan.Target != "ops@10.0.0.5" {
		t.Errorf("inventory host name did not resolve to its target: %q", plan.Target)
	}
}

func TestBuildTunnelPlanRejectsUnsafeOrAmbiguousSpecs(t *testing.T) {
	pick := func() (int, error) { return 1, nil }
	cases := map[string]tunnelSpec{
		"-bad":    {Host: "h", Socks: "1080"},
		"nohost":  {Socks: "1080"},
		"dash":    {Host: "-oProxyCommand=x", Socks: "1080"},
		"twokind": {Host: "h", Local: "1", Remote: "a:1", Socks: "1080"},
		"nolocal": {Host: "h", Remote: "a:1"},
		"badport": {Host: "h", Local: "70000", Remote: "a:1"},
		"badhost": {Host: "h", Local: "1", Remote: "-a:1"},
		"revauto": {Host: "h", Reverse: "auto:a:1"},
		"revloc":  {Host: "h", Local: "1", Reverse: "1:a:1"},
	}
	for name, spec := range cases {
		if _, err := buildTunnelPlan(name, spec, nil, pick); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestTunnelManagerUpRecordsStateAndDownRemovesIt(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	spec := tunnelSpec{Host: "jump1", Local: "auto", Remote: "db:5432"}
	row, err := manager.up(context.Background(), "db", spec)
	if err != nil {
		t.Fatal(err)
	}
	if row.Port != 40001 || row.State != tunnelStateUp {
		t.Fatalf("row = %+v", row)
	}
	if len(runner.started) != 1 || runner.started[0].Target != "ops@10.0.0.5" {
		t.Fatalf("started = %+v", runner.started)
	}
	if _, err := manager.up(context.Background(), "db", spec); err == nil {
		t.Fatal("second up must refuse while the tunnel is running")
	}
	rows := manager.rows(context.Background(), map[string]tunnelSpec{"db": spec, "idle": {Host: "jump1", Socks: "1080"}})
	if len(rows) != 2 || rows[0].Name != "db" || rows[0].State != tunnelStateUp || rows[0].Reach != tunnelReachOK {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[1].State != tunnelStateDown || !rows[1].Defined {
		t.Fatalf("idle row = %+v", rows[1])
	}
	stopped, err := manager.down(context.Background(), "db")
	if err != nil || !stopped || len(runner.stopped) != 1 {
		t.Fatalf("down = %v, %v, stopped %v", stopped, err, runner.stopped)
	}
	if records := manager.store.load(); len(records) != 0 {
		t.Fatalf("records = %+v", records)
	}
}

func TestTunnelManagerTreatsDeadMasterAsStaleAndReplacesIt(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	spec := tunnelSpec{Host: "jump1", Socks: "1080"}
	if _, err := manager.up(context.Background(), "proxy", spec); err != nil {
		t.Fatal(err)
	}
	runner.alive = map[string]bool{}
	rows := manager.rows(context.Background(), nil)
	if len(rows) != 1 || rows[0].State != tunnelStateStale || rows[0].Defined {
		t.Fatalf("rows = %+v", rows)
	}
	if _, err := manager.up(context.Background(), "proxy", spec); err != nil {
		t.Fatalf("a stale record must not block a new start: %v", err)
	}
}

func TestTunnelManagerKeepsNoRecordWhenSSHFailsToStart(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	runner.failure = errors.New("bind: Address already in use")
	if _, err := manager.up(context.Background(), "db", tunnelSpec{Host: "jump1", Local: "5432", Remote: "db:5432"}); err == nil {
		t.Fatal("want the start failure")
	}
	if records := manager.store.load(); len(records) != 0 {
		t.Fatalf("records = %+v", records)
	}
}

func TestTunnelManagerReportsBusyPortBeforeStarting(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	manager.available = func(port int) error { return tunnelError("tunnel.error.port_busy", port) }
	if _, err := manager.up(context.Background(), "db", tunnelSpec{Host: "jump1", Local: "5432", Remote: "db:5432"}); err == nil {
		t.Fatal("want the busy port error")
	}
	if len(runner.started) != 0 {
		t.Fatal("ssh must not start when the port is busy")
	}
}

func TestParseTunnelAdhocAndName(t *testing.T) {
	spec, err := parseTunnelAdhoc("jump1", "auto:db.internal:5432", "", "")
	if err != nil || spec.Local != "auto" || spec.Remote != "db.internal:5432" {
		t.Fatalf("spec = %+v, err %v", spec, err)
	}
	if name := tunnelAdhocName("ops@jump1", tunnelSpec{Local: "5432"}); name != "ops-jump1-5432" {
		t.Errorf("name = %q", name)
	}
	for _, test := range []struct {
		host string
		spec tunnelSpec
		want string
	}{
		{"jump1", tunnelSpec{Local: "auto", Remote: "db1:5432"}, "jump1-db1-5432"},
		{"jump1", tunnelSpec{Local: "auto", Remote: "db2:5432"}, "jump1-db2-5432"},
		{"jump1", tunnelSpec{Local: "auto", Remote: "[::1]:5432"}, "jump1-1-5432"},
		{"jump1", tunnelSpec{Socks: "auto"}, "jump1-dauto"},
		{"jump1", tunnelSpec{Reverse: "8080:localhost:3000"}, "jump1-r8080"},
		{"ops@bastion.prod.example.com", tunnelSpec{Local: "auto", Remote: "db.internal:5432"}, "ops-bast-db.internal-5432-9a8199"},
		{"jump1", tunnelSpec{Local: "auto", Remote: "a-very-long-database-host.internal.example.com:5432"}, "internal.example.com-5432-174749"},
	} {
		name := tunnelAdhocName(test.host, test.spec)
		if name != test.want {
			t.Errorf("tunnelAdhocName(%q, %+v) = %q, want %q", test.host, test.spec, name, test.want)
		}
		if len(name) > tunnelNameLimit || !tunnelNamePattern.MatchString(name) {
			t.Errorf("name %q is not valid", name)
		}
	}
	one := tunnelAdhocName("jump1", tunnelSpec{Local: "auto", Remote: "one-abcdefghijklmnopqrstuvwx.example:5432"})
	two := tunnelAdhocName("jump1", tunnelSpec{Local: "auto", Remote: "two-abcdefghijklmnopqrstuvwx.example:5432"})
	if one == two || len(one) > tunnelNameLimit || !tunnelNamePattern.MatchString(one) || !tunnelNamePattern.MatchString(two) {
		t.Errorf("destinations that differ only in the cut part must get different names: %q, %q", one, two)
	}
	for length := 1; length <= 2*tunnelNameLimit; length++ {
		remote := strings.Repeat("d", length) + ":5432"
		name := tunnelAdhocName("bastion-host", tunnelSpec{Local: "auto", Remote: remote})
		if len(name) > tunnelNameLimit || !tunnelNamePattern.MatchString(name) {
			t.Errorf("destination %q: name %q is not valid", remote, name)
		}
	}
	if _, err := parseTunnelAdhoc("h", "1:a:1", "", "1080"); err == nil {
		t.Error("two forward kinds must fail")
	}
	if _, err := parseTunnelAdhoc("h", "5432", "", ""); err == nil {
		t.Error("-L without a destination must fail")
	}
}

func TestSaveAndRemoveTunnelDefinitionKeepComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".edc", "tunnels.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "# my tunnels\ntunnels:\n  # database\n  db:\n    host: jump1\n    local: 5432\n    remote: db.internal:5432\n"
	if err := os.WriteFile(path, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := saveTunnelDefinition(path, "proxy", tunnelSpec{Host: "jump1", Socks: "1080"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	for _, want := range []string{"# my tunnels", "# database", "socks: 1080", "local: 5432"} {
		if !strings.Contains(text, want) {
			t.Errorf("saved file misses %q:\n%s", want, text)
		}
	}
	specs, err := loadTunnelDefinitions(path, nil)
	if err != nil || len(specs) != 2 || specs["proxy"].Socks != "1080" {
		t.Fatalf("specs = %+v, err %v", specs, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
	if err := saveTunnelDefinition(path, "proxy", tunnelSpec{Host: "x", Socks: "1"}); err == nil {
		t.Error("duplicate name must fail")
	}
	if err := removeTunnelDefinition(path, "db"); err != nil {
		t.Fatal(err)
	}
	specs, err = loadTunnelDefinitions(path, nil)
	if err != nil || len(specs) != 1 || specs["proxy"].Host != "jump1" {
		t.Fatalf("specs after remove = %+v, err %v", specs, err)
	}
	if err := removeTunnelDefinition(path, "missing"); err == nil {
		t.Error("removing an unknown tunnel must fail")
	}
}

func TestSaveTunnelDefinitionCreatesFileWithPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".edc", "tunnels.yaml")
	if err := saveTunnelDefinition(path, "db", tunnelSpec{Host: "jump1", Local: "auto", Remote: "db:5432"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("stat = %v, %v", info, err)
	}
	if _, err := loadTunnelDefinitions(path, nil); err != nil {
		t.Fatal(err)
	}
}

func TestLoadTunnelDefinitionsRejectsInvalidEntryAndUnknownField(t *testing.T) {
	dir := t.TempDir()
	invalid := filepath.Join(dir, "a.yaml")
	os.WriteFile(invalid, []byte("tunnels:\n  db:\n    host: h\n"), 0o600)
	if _, err := loadTunnelDefinitions(invalid, nil); err == nil {
		t.Error("entry without a forward must fail")
	}
	unknown := filepath.Join(dir, "b.yaml")
	os.WriteFile(unknown, []byte("tunnels:\n  db:\n    host: h\n    socks: 1\n    typo: 2\n"), 0o600)
	if _, err := loadTunnelDefinitions(unknown, nil); err == nil {
		t.Error("unknown field must fail")
	}
}

func TestTunnelSocketPathRejectsLongPaths(t *testing.T) {
	store := tunnelStore{dir: "/" + strings.Repeat("a", tunnelSocketPathLimit)}
	if _, err := store.socketPath("db"); err == nil {
		t.Error("a path over the unix socket limit must fail")
	}
}

func TestTunnelFormBuildsSpecForEachKind(t *testing.T) {
	form := tunnelForm{host: "jump1", kind: tunnelReverse, port: "8080", destination: "localhost:3000"}
	name, spec := form.spec()
	if spec.Reverse != "8080:localhost:3000" || name != "jump1-r8080" {
		t.Errorf("reverse: %q %+v", name, spec)
	}
	form = tunnelForm{host: "jump1", kind: tunnelSocks, port: "auto"}
	if _, spec = form.spec(); spec.Socks != "auto" {
		t.Errorf("socks: %+v", spec)
	}
	for _, field := range form.visibleFields() {
		if field == tunnelFieldDestination {
			t.Errorf("socks must hide the destination: %v", form.visibleFields())
		}
	}
}

func TestTunnelCommandLineMatchesManualSSH(t *testing.T) {
	targets := map[string]string{"jump1": "ops@10.0.0.5"}
	pick := func() (int, error) { return 40001, nil }
	cases := []struct {
		spec tunnelSpec
		want string
	}{
		{tunnelSpec{Host: "jump1", Local: "5432", Remote: "db.internal:5432"}, "ssh -N -L 127.0.0.1:5432:db.internal:5432 ops@10.0.0.5"},
		{tunnelSpec{Host: "dev", Reverse: "8080:localhost:3000"}, "ssh -N -R 8080:localhost:3000 dev"},
		{tunnelSpec{Host: "jump1", Socks: "auto"}, "ssh -N -D 127.0.0.1:40001 ops@10.0.0.5"},
	}
	for _, test := range cases {
		plan, err := buildTunnelPlan("t", test.spec, targets, pick)
		if err != nil || plan.Command() != test.want {
			t.Errorf("command = %q, err %v, want %q", plan.Command(), err, test.want)
		}
	}
	if got := tunnelCommandLine("a b", []string{"-D", "x'y"}); got != `ssh -N -D 'x'\''y' 'a b'` {
		t.Errorf("quoted command = %s", got)
	}
}

func TestTunnelRowsCarryTheCommandForRunningAndDefinedTunnels(t *testing.T) {
	manager, _ := newTestTunnelManager(t)
	specs := map[string]tunnelSpec{
		"db":   {Host: "jump1", Local: "auto", Remote: "db:5432"},
		"idle": {Host: "jump1", Local: "auto", Remote: "db:5432"},
	}
	if _, err := manager.up(context.Background(), "db", specs["db"]); err != nil {
		t.Fatal(err)
	}
	rows := manager.rows(context.Background(), specs)
	if rows[0].Command != "ssh -N -L 127.0.0.1:40001:db:5432 ops@10.0.0.5" {
		t.Errorf("running command = %q", rows[0].Command)
	}
	if rows[1].Command != "" {
		t.Errorf("an auto port that is not chosen yet must not print a command: %q", rows[1].Command)
	}
	var output strings.Builder
	printTunnelRows(&output, rows, manager.now(), true)
	if !strings.Contains(output.String(), "COMMAND") || !strings.Contains(output.String(), "ssh -N -L 127.0.0.1:40001") {
		t.Errorf("verbose list misses the command:\n%s", output.String())
	}
}

func TestTunnelManagerRetriesAnAutoPortThatWasTakenInTheMeantime(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	next := 40000
	manager.pick = func() (int, error) { next++; return next, nil }
	runner.failures = []error{errors.New("bind [127.0.0.1]:40001: Address already in use")}
	row, err := manager.up(context.Background(), "db", tunnelSpec{Host: "jump1", Local: "auto", Remote: "db:5432"})
	if err != nil || row.Port != 40002 || runner.attempts != 2 {
		t.Fatalf("row %+v, err %v, attempts %d", row, err, runner.attempts)
	}
}

func TestTunnelManagerDoesNotRetryOtherFailuresOrFixedPorts(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	runner.failure = errors.New("Permission denied (publickey)")
	if _, err := manager.up(context.Background(), "db", tunnelSpec{Host: "jump1", Local: "auto", Remote: "db:5432"}); err == nil || runner.attempts != 1 {
		t.Fatalf("an auth failure must stop at once: err %v, attempts %d", err, runner.attempts)
	}
	runner.failure, runner.attempts = errors.New("Address already in use"), 0
	if _, err := manager.up(context.Background(), "db", tunnelSpec{Host: "jump1", Local: "5432", Remote: "db:5432"}); err == nil || runner.attempts != 1 {
		t.Fatalf("a fixed port must not be replaced: err %v, attempts %d", err, runner.attempts)
	}
}

func TestTunnelManagerNamesTheTunnelThatHoldsThePort(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	ctx := context.Background()
	if _, err := manager.up(ctx, "db", tunnelSpec{Host: "jump1", Local: "5432", Remote: "db:5432"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.up(ctx, "other", tunnelSpec{Host: "jump1", Socks: "5432"}); err == nil || !strings.Contains(err.Error(), "db") {
		t.Fatalf("a local port clash must name db: %v", err)
	}
	if _, err := manager.up(ctx, "rev1", tunnelSpec{Host: "jump1", Reverse: "8080:localhost:3000"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.up(ctx, "rev2", tunnelSpec{Host: "jump1", Reverse: "8080:localhost:4000"}); err == nil || !strings.Contains(err.Error(), "rev1") {
		t.Fatalf("a remote port clash on the same host must name rev1: %v", err)
	}
	if _, err := manager.up(ctx, "rev3", tunnelSpec{Host: "dev", Reverse: "8080:localhost:3000"}); err != nil {
		t.Fatalf("the same remote port on another host is not a clash: %v", err)
	}
	if _, err := manager.up(ctx, "same", tunnelSpec{Host: "jump1", Socks: "7000"}); err != nil {
		t.Fatalf("a reverse port and a local port do not clash: %v", err)
	}
	runner.alive = map[string]bool{}
	if _, err := manager.up(ctx, "again", tunnelSpec{Host: "jump1", Local: "5432", Remote: "db:5432"}); err != nil {
		t.Fatalf("a dead tunnel must not hold its port: %v", err)
	}
}

func TestTunnelManagerExplainsABusyRemotePort(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	runner.failure = errors.New("Error: remote port forwarding failed for listen port 22")
	_, err := manager.up(context.Background(), "rev", tunnelSpec{Host: "jump1", Reverse: "22:localhost:3000"})
	if err == nil || !strings.Contains(err.Error(), "remote port forwarding failed") || !strings.Contains(err.Error(), "22") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), T("tunnel.hint.remote_port", 22)) {
		t.Fatalf("the failure must carry the hint: %v", err)
	}
}

func TestTunnelManagerWarnsWhenTheReverseTargetIsNotListening(t *testing.T) {
	manager, _ := newTestTunnelManager(t)
	manager.dial = func(string) error { return errors.New("connection refused") }
	row, err := manager.up(context.Background(), "rev", tunnelSpec{Host: "jump1", Reverse: "8080:localhost:3000"})
	if err != nil || len(row.Warnings) != 1 || !strings.Contains(row.Warnings[0], "localhost:3000") {
		t.Fatalf("row %+v, err %v", row, err)
	}
	manager.dial = func(string) error { return nil }
	row, err = manager.up(context.Background(), "rev2", tunnelSpec{Host: "jump1", Reverse: "8081:localhost:3000"})
	if err != nil || len(row.Warnings) != 0 {
		t.Fatalf("row %+v, err %v", row, err)
	}
}

func TestTunnelSharedWarningsFindDefinitionsThatUseTheSamePort(t *testing.T) {
	specs := map[string]tunnelSpec{
		"db":    {Host: "jump1", Local: "5432", Remote: "db:5432"},
		"db2":   {Host: "jump1", Local: "5432", Remote: "db2:5432"},
		"rev":   {Host: "jump1", Reverse: "5432:localhost:1"},
		"rev2":  {Host: "jump1", Reverse: "5432:localhost:2"},
		"rev3":  {Host: "other", Reverse: "5432:localhost:3"},
		"proxy": {Host: "jump1", Socks: "auto"},
	}
	if warnings := tunnelSharedWarnings("db", specs, nil); len(warnings) != 1 || !strings.Contains(warnings[0], "db2") {
		t.Errorf("db: %v", warnings)
	}
	if warnings := tunnelSharedWarnings("rev", specs, nil); len(warnings) != 1 || !strings.Contains(warnings[0], "rev2") {
		t.Errorf("rev: %v", warnings)
	}
	if warnings := tunnelSharedWarnings("proxy", specs, nil); len(warnings) != 0 {
		t.Errorf("an auto port has nothing to share: %v", warnings)
	}
}

func checkSteps(checks []tunnelCheck) string {
	parts := make([]string, len(checks))
	for index, check := range checks {
		parts[index] = check.Step + ":" + string(check.Status)
	}
	return strings.Join(parts, " ")
}

func TestTunnelCheckLocalSeparatesLoginFromDestination(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	runner.probe = func(args []string) (string, error) {
		if len(args) > 0 && args[0] == "-W" {
			return "Stdio forwarding request failed: Session open refused by peer", errors.New("exit status 255")
		}
		return "", nil
	}
	checks, err := manager.check(context.Background(), "db", tunnelSpec{Host: "jump1", Local: "5432", Remote: "db:5432"})
	if err != nil {
		t.Fatal(err)
	}
	if got := checkSteps(checks); got != "local_port:pass login:pass destination:fail" {
		t.Fatalf("steps = %s", got)
	}
	if !strings.Contains(checks[2].Detail, "Session open refused by peer") {
		t.Errorf("detail = %q", checks[2].Detail)
	}
	if len(runner.probes) != 2 || strings.Join(runner.probes[0], " ") != "ops@10.0.0.5 true" ||
		strings.Join(runner.probes[1], " ") != "-W db:5432 ops@10.0.0.5" {
		t.Errorf("probes = %v", runner.probes)
	}
	if tunnelFailedChecks(checks) != 1 || runner.attempts != 0 {
		t.Errorf("a check must not start a tunnel: failed %d, attempts %d", tunnelFailedChecks(checks), runner.attempts)
	}
}

func TestTunnelCheckSkipsHostStepsWhenLoginFails(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	runner.probe = func([]string) (string, error) {
		return "Warning: noise\nroot@10.0.0.5: Permission denied (publickey).", errors.New("exit status 255")
	}
	checks, _ := manager.check(context.Background(), "db", tunnelSpec{Host: "jump1", Local: "5432", Remote: "db:5432"})
	if got := checkSteps(checks); got != "local_port:pass login:fail destination:skip" {
		t.Fatalf("steps = %s", got)
	}
	if !strings.Contains(checks[1].Detail, "Permission denied") || len(runner.probes) != 1 {
		t.Errorf("detail %q, probes %v", checks[1].Detail, runner.probes)
	}
}

func TestTunnelCheckReportsBusyLocalPortWithoutStoppingTheRest(t *testing.T) {
	manager, _ := newTestTunnelManager(t)
	manager.available = func(port int) error { return tunnelError("tunnel.error.port_busy", port) }
	checks, _ := manager.check(context.Background(), "db", tunnelSpec{Host: "jump1", Local: "5432", Remote: "db:5432"})
	if got := checkSteps(checks); got != "local_port:fail login:pass destination:pass" {
		t.Fatalf("steps = %s", got)
	}
}

func TestTunnelCheckReverseProbesTheRemotePortAndWarnsAboutTheTarget(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	manager.dial = func(string) error { return errors.New("refused") }
	checks, _ := manager.check(context.Background(), "rev", tunnelSpec{Host: "jump1", Reverse: "8080:localhost:3000"})
	if got := checkSteps(checks); got != "login:pass remote_port:pass target:warn" {
		t.Fatalf("steps = %s", got)
	}
	want := "-o ExitOnForwardFailure=yes -R 8080:localhost:3000 ops@10.0.0.5 true"
	if len(runner.probes) != 2 || strings.Join(runner.probes[1], " ") != want {
		t.Errorf("probes = %v", runner.probes)
	}
	if !strings.Contains(tunnelCheckSummary(checks), T("tunnel.check.summary_warn")) {
		t.Errorf("summary = %q", tunnelCheckSummary(checks))
	}
	runner.probe = func(args []string) (string, error) {
		if len(args) > 1 && args[1] == "ExitOnForwardFailure=yes" {
			return "Error: remote port forwarding failed for listen port 8080", errors.New("exit status 255")
		}
		return "", nil
	}
	checks, _ = manager.check(context.Background(), "rev", tunnelSpec{Host: "jump1", Reverse: "8080:localhost:3000"})
	if got := checkSteps(checks); got != "login:pass remote_port:fail target:warn" {
		t.Fatalf("steps = %s", got)
	}
}

func TestTunnelCheckOfARunningTunnelDoesNotRebindItsPorts(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	spec := tunnelSpec{Host: "jump1", Reverse: "8080:localhost:3000"}
	if _, err := manager.up(context.Background(), "rev", spec); err != nil {
		t.Fatal(err)
	}
	checks, _ := manager.check(context.Background(), "rev", spec)
	if got := checkSteps(checks); got != "login:pass remote_port:pass target:pass" {
		t.Fatalf("steps = %s", got)
	}
	for _, probe := range runner.probes {
		if strings.Contains(strings.Join(probe, " "), "-R") {
			t.Errorf("a running tunnel must not be probed with -R again: %v", probe)
		}
	}
}

func TestTunnelRecordRebuildsItsSpec(t *testing.T) {
	cases := []struct {
		record tunnelRecord
		plan   string
	}{
		{tunnelRecord{Name: "a", Host: "jump1", Kind: tunnelLocal, Port: 6000, Destination: "db:5432"}, "-L 127.0.0.1:6000:db:5432"},
		{tunnelRecord{Name: "b", Host: "jump1", Kind: tunnelReverse, Port: 8080, Destination: "localhost:3000"}, "-R 8080:localhost:3000"},
		{tunnelRecord{Name: "c", Host: "jump1", Kind: tunnelSocks, Port: 1080}, "-D 127.0.0.1:1080"},
	}
	for _, test := range cases {
		plan, err := buildTunnelPlan(test.record.Name, test.record.spec(), nil, func() (int, error) { return 0, nil })
		if err != nil || strings.Join(plan.Forward, " ") != test.plan {
			t.Errorf("%s: forward %v, err %v, want %s", test.record.Name, plan.Forward, err, test.plan)
		}
	}
}

func captureTunnelStdout(t *testing.T, run func() int) (string, int) {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	status := run()
	writer.Close()
	os.Stdout = original
	data, _ := io.ReadAll(reader)
	return string(data), status
}

func TestRunTunnelUpSavesAnAdhocTunnelOnlyWithSave(t *testing.T) {
	manager, _ := newTestTunnelManager(t)
	file := filepath.Join(t.TempDir(), "tunnels.yaml")
	environment := tunnelEnvironment{manager: manager, specs: map[string]tunnelSpec{}, file: file}
	adhoc := [3]string{"6000:db:5432", "", ""}
	if _, status := captureTunnelStdout(t, func() int {
		return runTunnelUp(context.Background(), environment, []string{"jump1"}, "once", adhoc, false, 0)
	}); status != 0 {
		t.Fatalf("status = %d", status)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("up without --save must not write the file")
	}
	output, status := captureTunnelStdout(t, func() int {
		return runTunnelUp(context.Background(), environment, []string{"jump1"}, "kept", [3]string{"6001:db:5432", "", ""}, true, 0)
	})
	if status != 0 || !strings.Contains(output, "kept") {
		t.Fatalf("status %d, output %q", status, output)
	}
	specs, err := loadTunnelDefinitions(file, nil)
	if err != nil || specs["kept"].Remote != "db:5432" || specs["kept"].Local != "6001" || len(specs) != 1 {
		t.Fatalf("specs = %+v, err %v", specs, err)
	}
}

func TestRunTunnelUpSaveRefusesNamedTunnelsAndDuplicates(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	environment := tunnelEnvironment{manager: manager, file: filepath.Join(t.TempDir(), "tunnels.yaml"),
		specs: map[string]tunnelSpec{"db": {Host: "jump1", Local: "5432", Remote: "db:5432"}}}
	if status := runTunnelUp(context.Background(), environment, []string{"db"}, "", [3]string{}, true, 0); status != 2 {
		t.Errorf("--save with a defined name = %d", status)
	}
	if status := runTunnelUp(context.Background(), environment, []string{"jump1"}, "db", [3]string{"6000:x:1", "", ""}, true, 0); status != 2 {
		t.Errorf("--save over an existing name = %d", status)
	}
	if len(runner.started) != 0 {
		t.Error("nothing may start when the save is refused")
	}
}

func TestRemotePortTakenMessageFormatsInEveryLanguage(t *testing.T) {
	previous := currentLanguage()
	defer setLanguage(previous)
	for _, language := range supportedLanguages {
		setLanguage(language)
		message := T("tunnel.error.remote_port_taken", 18081, "root@jw-server", "rev-a")
		for _, want := range []string{"18081", "root@jw-server", "rev-a"} {
			if !strings.Contains(message, want) {
				t.Errorf("%s: %q misses %q", language, message, want)
			}
		}
		if strings.Contains(message, "%!") {
			t.Errorf("%s: the verbs do not match the arguments: %q", language, message)
		}
	}
}

func TestRunTunnelDownRejectsAllWithNames(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	if _, err := manager.up(context.Background(), "proxy", tunnelSpec{Host: "jump1", Socks: "1080"}); err != nil {
		t.Fatal(err)
	}
	environment := tunnelEnvironment{manager: manager, specs: map[string]tunnelSpec{}}
	if status := runTunnelDown(context.Background(), environment, []string{"proxy"}, true); status != 2 {
		t.Fatalf("status = %d", status)
	}
	if len(runner.stopped) != 0 {
		t.Fatal("a rejected command must stop nothing")
	}
	if _, status := captureTunnelStdout(t, func() int {
		return runTunnelDown(context.Background(), environment, nil, true)
	}); status != 0 || len(runner.stopped) != 1 {
		t.Fatalf("--all alone must stop every tunnel: status %d, stopped %v", status, runner.stopped)
	}
}

func TestTunnelManagerStopsTheSSHMasterWhenItCannotSaveTheRecord(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	if err := os.MkdirAll(manager.store.recordPath("db"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.up(context.Background(), "db", tunnelSpec{Host: "jump1", Local: "5432", Remote: "db:5432"}); err == nil {
		t.Fatal("a record that cannot be saved must fail the start")
	}
	if len(runner.started) != 1 || len(runner.stopped) != 1 {
		t.Fatalf("the master that started must be stopped: started %d, stopped %v", len(runner.started), runner.stopped)
	}
}

func TestTunnelManagerKeepsTheRecordWhenStopFails(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	if _, err := manager.up(context.Background(), "db", tunnelSpec{Host: "jump1", Local: "5432", Remote: "db:5432"}); err != nil {
		t.Fatal(err)
	}
	runner.stopFailure = errors.New("control socket busy")
	stopped, err := manager.down(context.Background(), "db")
	if err == nil || stopped {
		t.Fatalf("down = %v, %v", stopped, err)
	}
	if records := manager.store.load(); len(records) != 1 {
		t.Fatalf("a running master must stay discoverable: %+v", records)
	}
	runner.stopFailure = nil
	if stopped, err := manager.down(context.Background(), "db"); err != nil || !stopped {
		t.Fatalf("a retry must clean up: %v, %v", stopped, err)
	}
}

func TestNewTunnelEnvironmentAllowsAMissingFileOnlyForSave(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	missing := filepath.Join(t.TempDir(), "new", "tunnels.yaml")
	if _, err := newTunnelEnvironment(missing, time.Second, false); err == nil {
		t.Error("a missing --file must stay an error without --save")
	}
	environment, err := newTunnelEnvironment(missing, time.Second, true)
	if err != nil || environment.file != missing || len(environment.specs) != 0 {
		t.Fatalf("environment %+v, err %v", environment, err)
	}
	if err := saveTunnelDefinition(environment.file, "db", tunnelSpec{Host: "jump1", Local: "auto", Remote: "db:5432"}); err != nil {
		t.Fatalf("the first save must create the file and its directory: %v", err)
	}
}

func TestBuildTunnelPlanAcceptsABracketedIPv6ReverseTarget(t *testing.T) {
	pick := func() (int, error) { return 0, nil }
	plan, err := buildTunnelPlan("v6", tunnelSpec{Host: "jump1", Reverse: "8080:[::1]:3000"}, nil, pick)
	if err != nil || plan.Destination != "[::1]:3000" || strings.Join(plan.Forward, " ") != "-R 8080:[::1]:3000" || plan.Port != 8080 {
		t.Fatalf("plan %+v, err %v", plan, err)
	}
	if got := plan.Command(); got != "ssh -N -R 8080:[::1]:3000 jump1" {
		t.Errorf("command = %q", got)
	}
	for _, invalid := range []string{"8080:::1:3000", "8080:[::1]", "8080:[::1]:0", "8080:-x:1", "x:[::1]:3000"} {
		if _, err := buildTunnelPlan("v6", tunnelSpec{Host: "jump1", Reverse: invalid}, nil, pick); err == nil {
			t.Errorf("%q must be rejected", invalid)
		}
	}
	record := tunnelRecord{Name: "v6", Host: "jump1", Kind: tunnelReverse, Port: 8080, Destination: "[::1]:3000"}
	if again, err := buildTunnelPlan("v6", record.spec(), nil, pick); err != nil || again.Destination != "[::1]:3000" {
		t.Errorf("a record must rebuild its IPv6 spec: %+v, %v", again, err)
	}
}

// 가짜 ssh를 PATH에 두고 runTunnel을 끝까지 실행한다. 인자 해석, 환경 구성, 시작, 저장이 이어진 CLI 경로를 시험한다.
func TestRunTunnelUpSaveCreatesTheFirstDefinitionFileFromTheCLI(t *testing.T) {
	bin := shortTunnelDir(t)
	script := "#!/bin/sh\nfor argument in \"$@\"; do\n  if [ \"$argument\" = check ]; then echo 'Master running (pid=4242)' >&2; fi\ndone\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", shortTunnelDir(t))
	file := filepath.Join(t.TempDir(), "new", "tunnels.yaml")
	args := []string{"up", "--file", file, "-L", "auto:db.internal:5432", "--name", "first", "--save", "jump1"}
	output, status := captureTunnelStdout(t, func() int { return runTunnel(args, "test") })
	if status != 0 || !strings.Contains(output, "saved: first") {
		t.Fatalf("status %d, output %q", status, output)
	}
	specs, err := loadTunnelDefinitions(file, nil)
	if err != nil || specs["first"].Remote != "db.internal:5432" || specs["first"].Local != "auto" {
		t.Fatalf("specs = %+v, err %v", specs, err)
	}
	args = []string{"up", "--file", filepath.Join(t.TempDir(), "absent.yaml"), "-L", "auto:db:1", "--name", "second", "jump1"}
	if _, status := captureTunnelStdout(t, func() int { return runTunnel(args, "test") }); status != 2 {
		t.Fatalf("a missing --file must stay an error without --save: %d", status)
	}
}

// 상태 파일을 저장하지 못하면 터널을 끝낸다. 끝내지도 못하면 사용자가 직접 끝낼 명령을 알려야 한다.
func TestTunnelUpWhenTheStateFileCannotBeSaved(t *testing.T) {
	spec := tunnelSpec{Host: "jump1", Local: "auto", Remote: "db:5432"}
	block := func(t *testing.T, manager tunnelManager) {
		t.Helper()
		// 기록이 들어갈 자리를 비어 있지 않은 디렉터리로 막아 저장을 실패시킨다.
		if err := os.MkdirAll(filepath.Join(manager.store.recordPath("db"), "keep"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("the master stops", func(t *testing.T) {
		manager, runner := newTestTunnelManager(t)
		block(t, manager)
		_, err := manager.up(context.Background(), "db", spec)
		if err == nil || strings.Contains(err.Error(), "ssh -S") || len(runner.stopped) != 1 {
			t.Fatalf("err = %v, stopped = %v", err, runner.stopped)
		}
	})
	t.Run("the master does not stop", func(t *testing.T) {
		manager, runner := newTestTunnelManager(t)
		runner.stopFailure = errors.New("control socket is gone")
		block(t, manager)
		_, err := manager.up(context.Background(), "db", spec)
		if err == nil || !strings.Contains(err.Error(), "ssh -S") || !strings.Contains(err.Error(), "ops@10.0.0.5") || !strings.Contains(err.Error(), "control socket is gone") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRunTunnelRejectsSaveOutsideUp(t *testing.T) {
	for _, subcommand := range []string{"test", "list", "down"} {
		args := []string{subcommand, "--save", "-L", "6000:db:5432", "jump1"}
		stderr := captureTraceStderr(t, func() {
			if status := runTunnel(args, "test"); status != 2 {
				t.Errorf("%s --save: status = %d, want 2", subcommand, status)
			}
		})
		if !strings.Contains(stderr, "--save works only with edc tunnel up") {
			t.Errorf("%s --save: stderr = %q", subcommand, stderr)
		}
	}
}

// 가짜 ssh로 `edc tunnel test`를 CLI 경로로 끝까지 실행한다. 대상 해석, 점검 순서, 종료 코드를 본다.
func TestRunTunnelTestRunsThroughTheCLI(t *testing.T) {
	bin := shortTunnelDir(t)
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", shortTunnelDir(t))
	args := []string{"test", "--json", "-", "-L", "auto:db.internal:5432", "--name", "probe", "jump1"}
	output, status := captureTunnelStdout(t, func() int { return runTunnel(args, "test") })
	if status != 0 || !strings.Contains(output, `"ok": true`) || !strings.Contains(output, `"name": "probe"`) {
		t.Fatalf("status %d, output %q", status, output)
	}
	if _, status := captureTunnelStdout(t, func() int { return runTunnel([]string{"test", "nobody"}, "test") }); status != 2 {
		t.Fatalf("an unknown tunnel name must be an error: %d", status)
	}
}

func TestParseTunnelLineReadsShortFormsAndPastedSSHCommands(t *testing.T) {
	cases := []struct {
		line string
		want tunnelSpec
	}{
		{"jump1 db.internal:5432", tunnelSpec{Host: "jump1", Local: "auto", Remote: "db.internal:5432"}},
		{"db.internal:5432 jump1", tunnelSpec{Host: "jump1", Local: "auto", Remote: "db.internal:5432"}},
		{"ops@jump1 6000:db.internal:5432", tunnelSpec{Host: "ops@jump1", Local: "6000", Remote: "db.internal:5432"}},
		{"jump1 6000:[::1]:5432", tunnelSpec{Host: "jump1", Local: "6000", Remote: "[::1]:5432"}},
		{"ssh -L 5432:db.internal:5432 jump1", tunnelSpec{Host: "jump1", Local: "5432", Remote: "db.internal:5432"}},
		{"ssh -N -f -L 127.0.0.1:5432:db.internal:5432 jump1", tunnelSpec{Host: "jump1", Local: "5432", Remote: "db.internal:5432"}},
		{"ssh jump1 -Llocalhost:5432:db.internal:5432", tunnelSpec{Host: "jump1", Local: "5432", Remote: "db.internal:5432"}},
		{"ssh -fN -R 8080:localhost:3000 jump1", tunnelSpec{Host: "jump1", Reverse: "8080:localhost:3000"}},
		{"-D 1080 jump1", tunnelSpec{Host: "jump1", Socks: "1080"}},
		{"jump1 -D auto", tunnelSpec{Host: "jump1", Socks: "auto"}},
	}
	for _, test := range cases {
		spec, err := parseTunnelLine(test.line)
		if err != nil || spec != test.want {
			t.Errorf("%q: spec = %+v, err %v, want %+v", test.line, spec, err, test.want)
			continue
		}
		if _, err := buildTunnelPlan("probe", spec, nil, func() (int, error) { return 40001, nil }); err != nil {
			t.Errorf("%q: the spec does not build: %v", test.line, err)
		}
	}
}

func TestParseTunnelLineRejectsWhatATunnelCannotCarry(t *testing.T) {
	cases := []struct{ line, want string }{
		{"ssh -p 2222 -L 5432:db:5432 jump1", "-p"},
		{"ssh -i key.pem -L 5432:db:5432 jump1", "-i"},
		{"ssh -J bastion -L 5432:db:5432 jump1", "-J"},
		{"ssh -o StrictHostKeyChecking=no -L 5432:db:5432 jump1", "-o"},
		{"ssh -L 0.0.0.0:5432:db:5432 jump1", "0.0.0.0"},
		{"ssh -L 5432:db:5432 -D 1080 jump1", "exactly one"},
		{"jump1 db", "jump1 db.internal:5432"},
		{"jump1 db:5432 extra", "jump1 db.internal:5432"},
		{"ssh -L 5432:db:5432", "jump1 db.internal:5432"},
		{"ssh jump1 -L", "jump1 db.internal:5432"},
	}
	for _, test := range cases {
		if _, err := parseTunnelLine(test.line); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%q: err = %v, want it to name %q", test.line, err, test.want)
		}
	}
}

func TestParseTunnelAdhocAcceptsAMissingLocalPortAndALoopbackBind(t *testing.T) {
	for value, want := range map[string]tunnelPort{"db:5432": "auto", "127.0.0.1:6000:db:5432": "6000", "6000:db:5432": "6000"} {
		spec, err := parseTunnelAdhoc("jump1", value, "", "")
		if err != nil || spec.Local != want || spec.Remote != "db:5432" {
			t.Errorf("-L %s: spec = %+v, err %v", value, spec, err)
		}
	}
	if _, err := parseTunnelAdhoc("jump1", "10.0.0.1:6000:db:5432", "", ""); err == nil {
		t.Error("a bind address that edc cannot honor must fail")
	}
}

func TestTunnelManagerStartWithATimeLimitRecordsTheExpiryAndStartsTheTimer(t *testing.T) {
	manager, _ := newTestTunnelManager(t)
	var timed []tunnelRecord
	manager.expire = func(_ tunnelStore, record tunnelRecord) error {
		timed = append(timed, record)
		return nil
	}
	spec := tunnelSpec{Host: "jump1", Local: "auto", Remote: "db:5432"}
	row, err := manager.start(context.Background(), "db", spec, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	want := manager.now().Add(30 * time.Minute)
	if row.Expires == nil || !row.Expires.Equal(want) {
		t.Fatalf("row expires = %v, want %v", row.Expires, want)
	}
	record, found := manager.store.find("db")
	if !found || record.Expires == nil || !record.Expires.Equal(want) || record.TTL != 30*time.Minute || !record.Auto {
		t.Fatalf("record = %+v", record)
	}
	if len(timed) != 1 || timed[0].Name != "db" || !timed[0].Started.Equal(record.Started) {
		t.Fatalf("timer got %+v", timed)
	}
	rows := manager.rows(context.Background(), nil)
	if len(rows) != 1 || rows[0].Expires == nil || tunnelRemaining(rows[0].Expires, manager.now().Add(10*time.Minute)) != "20m0s" {
		t.Fatalf("rows = %+v", rows)
	}
	if _, err := manager.up(context.Background(), "plain", tunnelSpec{Host: "jump1", Local: "6000", Remote: "db:5432"}); err != nil {
		t.Fatal(err)
	}
	if plain, _ := manager.store.find("plain"); plain.Expires != nil || len(timed) != 1 {
		t.Fatalf("a tunnel without a time limit must not get a timer: %+v, timers %d", plain, len(timed))
	}
}

func TestTunnelManagerStopsTheTunnelWhenItsTimerCannotStart(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	manager.expire = func(tunnelStore, tunnelRecord) error { return errors.New("fork failed") }
	_, err := manager.start(context.Background(), "db", tunnelSpec{Host: "jump1", Local: "6000", Remote: "db:5432"}, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "fork failed") {
		t.Fatalf("err = %v", err)
	}
	if len(runner.stopped) != 1 || len(manager.store.load()) != 0 {
		t.Fatalf("a tunnel that nothing will stop must not stay: stopped %v, records %+v", runner.stopped, manager.store.load())
	}
}

func TestTunnelExpireWhenDueStopsOnlyTheTunnelItWasStartedFor(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	if _, err := manager.start(context.Background(), "db", tunnelSpec{Host: "jump1", Local: "6000", Remote: "db:5432"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	record, _ := manager.store.find("db")
	started := record.Started.UnixNano()
	clock := manager.now()
	manager.now = func() time.Time { return clock }
	var naps []time.Duration
	sleep := func(duration time.Duration) {
		naps = append(naps, duration)
		clock = clock.Add(duration)
	}
	if err := manager.expireWhenDue("db", started+1, sleep); err != nil || len(runner.stopped) != 0 || len(naps) != 0 {
		t.Fatalf("a timer for another start must leave the tunnel: err %v, stopped %v, naps %v", err, runner.stopped, naps)
	}
	if err := manager.expireWhenDue("db", started, sleep); err != nil {
		t.Fatal(err)
	}
	if len(runner.stopped) != 1 || len(manager.store.load()) != 0 {
		t.Fatalf("stopped %v, records %+v", runner.stopped, manager.store.load())
	}
	if len(naps) != 4 {
		t.Fatalf("one minute must be waited in short naps so that a sleeping machine cannot stall it: %v", naps)
	}
	for _, nap := range naps {
		if nap > tunnelExpiryPoll {
			t.Errorf("nap %v is longer than the poll interval", nap)
		}
	}
}

func TestTunnelManagerReportsATimedTunnelItCouldNotStop(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	manager.expire = func(tunnelStore, tunnelRecord) error { return errors.New("fork failed") }
	runner.stopFailure = errors.New("mux refused")
	_, err := manager.start(context.Background(), "db", tunnelSpec{Host: "jump1", Local: "6000", Remote: "db:5432"}, time.Minute)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"fork failed", "mux refused", "-O exit "} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must tell how to stop the tunnel by hand, missing %q: %v", want, err)
		}
	}
}

func TestTunnelExpireWhenDueLeavesATunnelRestartedAtTheLastMoment(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	spec := tunnelSpec{Host: "jump1", Local: "6000", Remote: "db:5432"}
	if _, err := manager.start(context.Background(), "db", spec, time.Minute); err != nil {
		t.Fatal(err)
	}
	record, _ := manager.store.find("db")
	due := record.Expires.Add(time.Second)
	restarted := false
	manager.now = func() time.Time {
		// 만료를 확인하는 바로 그 시점에 같은 이름으로 다시 시작된 경우다.
		if !restarted {
			restarted = true
			manager.now = func() time.Time { return due }
			if _, err := manager.down(context.Background(), "db"); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.start(context.Background(), "db", spec, time.Hour); err != nil {
				t.Fatal(err)
			}
		}
		return due
	}
	if err := manager.expireWhenDue("db", record.Started.UnixNano(), func(time.Duration) {}); err != nil {
		t.Fatal(err)
	}
	if len(runner.stopped) != 1 || len(manager.store.load()) != 1 {
		t.Fatalf("the old timer must not stop the restarted tunnel: stopped %v, records %+v", runner.stopped, manager.store.load())
	}
}

func TestTunnelDownWithoutAStateDirectoryStopsNothing(t *testing.T) {
	manager, _ := newTestTunnelManager(t)
	manager.store.dir = filepath.Join(t.TempDir(), "absent")
	if stopped, err := manager.down(context.Background(), "db"); stopped || err != nil {
		t.Fatalf("stopped %v, err %v", stopped, err)
	}
}

func TestTunnelManagerExtendsTheStopTime(t *testing.T) {
	manager, _ := newTestTunnelManager(t)
	timers := 0
	manager.expire = func(tunnelStore, tunnelRecord) error { timers++; return nil }
	spec := tunnelSpec{Host: "jump1", Local: "6000", Remote: "db:5432"}
	if _, err := manager.start(context.Background(), "timed", spec, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.start(context.Background(), "plain", tunnelSpec{Host: "jump1", Local: "6001", Remote: "db:5432"}, 0); err != nil {
		t.Fatal(err)
	}
	now := manager.now()
	started, _ := manager.store.find("timed")
	release, held, err := manager.store.holdExpiry("timed", started.Started.UnixNano())
	if err != nil || !held {
		t.Fatalf("held %v, err %v", held, err)
	}
	timed, err := manager.adjustExpiry("timed", 30*time.Minute)
	if err != nil || !timed.Expires.Equal(now.Add(90*time.Minute)) || timed.TTL != 90*time.Minute || timers != 1 {
		t.Fatalf("a timed tunnel must stop 30 minutes later with the same timer: %+v, err %v, timers %d", timed, err, timers)
	}
	// 보조 프로세스가 죽어 잠금이 풀렸으면, 시간을 옮길 때 새로 띄워야 터널이 끝난다.
	release()
	if _, err := manager.adjustExpiry("timed", 30*time.Minute); err != nil || timers != 2 {
		t.Fatalf("a timed tunnel without a live timer must get a new one: err %v, timers %d", err, timers)
	}
	timers = 1
	plain, err := manager.adjustExpiry("plain", 30*time.Minute)
	if err != nil || !plain.Expires.Equal(now.Add(30*time.Minute)) || timers != 2 {
		t.Fatalf("a tunnel without a limit must get one from now and a timer: %+v, err %v, timers %d", plain, err, timers)
	}
	if record, _ := manager.store.find("plain"); record.Expires == nil || !record.Expires.Equal(*plain.Expires) {
		t.Fatalf("the new stop time must be recorded: %+v", record)
	}
	if _, err := manager.adjustExpiry("nobody", 30*time.Minute); err == nil {
		t.Error("a tunnel that does not run must not be extended")
	}
}

func TestTunnelManagerKeepsNoStopTimeWhenItsTimerCannotStart(t *testing.T) {
	manager, _ := newTestTunnelManager(t)
	if _, err := manager.start(context.Background(), "plain", tunnelSpec{Host: "jump1", Local: "6001", Remote: "db:5432"}, 0); err != nil {
		t.Fatal(err)
	}
	manager.expire = func(tunnelStore, tunnelRecord) error { return errors.New("fork failed") }
	if _, err := manager.adjustExpiry("plain", 30*time.Minute); err == nil || !strings.Contains(err.Error(), "fork failed") {
		t.Fatalf("err = %v", err)
	}
	if record, _ := manager.store.find("plain"); record.Expires != nil || record.TTL != 0 {
		t.Fatalf("a stop time that nothing will act on must not stay: %+v", record)
	}
}

func TestTunnelExpireWhenDueWaitsAgainWhenTheStopTimeMovesAtTheLastMoment(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	if _, err := manager.start(context.Background(), "db", tunnelSpec{Host: "jump1", Local: "6000", Remote: "db:5432"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	record, _ := manager.store.find("db")
	clock := record.Expires.Add(time.Second)
	manager.now = func() time.Time { return clock }
	helper, calls := manager, 0
	helper.now = func() time.Time {
		calls++
		// 만료를 확인한 바로 뒤, 잠그기 전에 사용자가 늘린 경우다.
		if calls == 1 {
			if _, err := manager.adjustExpiry("db", 30*time.Minute); err != nil {
				t.Fatal(err)
			}
		}
		return clock
	}
	naps := 0
	sleep := func(duration time.Duration) {
		naps++
		if len(runner.stopped) != 0 {
			t.Fatal("the extended tunnel stopped before its new stop time")
		}
		clock = clock.Add(duration)
	}
	if err := helper.expireWhenDue("db", record.Started.UnixNano(), sleep); err != nil {
		t.Fatal(err)
	}
	if len(runner.stopped) != 1 || naps == 0 || clock.Sub(record.Expires.Add(time.Second)) < 30*time.Minute {
		t.Fatalf("stopped %v, naps %d, waited %v", runner.stopped, naps, clock.Sub(*record.Expires))
	}
}

func TestTunnelExpireWhenDueLeavesWhenTheTunnelStoppedEarlier(t *testing.T) {
	manager, runner := newTestTunnelManager(t)
	if _, err := manager.start(context.Background(), "db", tunnelSpec{Host: "jump1", Local: "6000", Remote: "db:5432"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	record, _ := manager.store.find("db")
	naps := 0
	sleep := func(time.Duration) {
		naps++
		if _, err := manager.down(context.Background(), "db"); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.expireWhenDue("db", record.Started.UnixNano(), sleep); err != nil || naps != 1 || len(runner.stopped) != 1 {
		t.Fatalf("err %v, naps %d, stopped %v", err, naps, runner.stopped)
	}
}

func TestTunnelRecordDefinitionKeepsAutoForTheSavedFile(t *testing.T) {
	record := tunnelRecord{Host: "jump1", Kind: tunnelLocal, Port: 40001, Destination: "db:5432", Auto: true}
	if spec := record.definition(); spec.Local != "auto" || spec.Remote != "db:5432" {
		t.Errorf("definition = %+v", spec)
	}
	if spec := record.spec(); spec.Local != "40001" {
		t.Errorf("the running tunnel keeps its port: %+v", spec)
	}
	record.Auto = false
	if spec := record.definition(); spec.Local != "40001" {
		t.Errorf("a fixed port stays fixed: %+v", spec)
	}
}

func TestRunTunnelUpPrintsWhenATimeLimitedTunnelStops(t *testing.T) {
	manager, _ := newTestTunnelManager(t)
	environment := tunnelEnvironment{manager: manager, specs: map[string]tunnelSpec{}}
	output, status := captureTunnelStdout(t, func() int {
		return runTunnelUp(context.Background(), environment, []string{"jump1"}, "", [3]string{"db:5432", "", ""}, false, 30*time.Minute)
	})
	if status != 0 || !strings.Contains(output, "stops automatically in 30m0s") {
		t.Fatalf("status %d, output %q", status, output)
	}
}

func TestRunTunnelUpWithATimeLimitRecordsItAndStartsItsTimer(t *testing.T) {
	bin := shortTunnelDir(t)
	script := "#!/bin/sh\nfor argument in \"$@\"; do\n  if [ \"$argument\" = check ]; then echo 'Master running (pid=4242)' >&2; fi\ndone\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", shortTunnelDir(t))
	t.Chdir(t.TempDir())
	var timed []tunnelRecord
	spawner := tunnelExpirySpawner
	tunnelExpirySpawner = func(_ tunnelStore, record tunnelRecord) error {
		timed = append(timed, record)
		return nil
	}
	t.Cleanup(func() { tunnelExpirySpawner = spawner })
	args := []string{"up", "--ttl", "30m", "-L", "db.internal:5432", "--name", "timed", "jump1"}
	output, status := captureTunnelStdout(t, func() int { return runTunnel(args, "test") })
	if status != 0 || !strings.Contains(output, "stops automatically in 30m0s") {
		t.Fatalf("status %d, output %q", status, output)
	}
	if len(timed) != 1 || timed[0].Name != "timed" || timed[0].TTL != 30*time.Minute {
		t.Fatalf("timers = %+v", timed)
	}
	store, err := defaultTunnelStore()
	if err != nil {
		t.Fatal(err)
	}
	record, found := store.find("timed")
	if !found || record.Expires == nil || !record.Expires.Equal(record.Started.Add(30*time.Minute)) {
		t.Fatalf("record = %+v, found %v", record, found)
	}
	output, status = captureTunnelStdout(t, func() int { return runTunnel([]string{"list"}, "test") })
	if status != 0 || !strings.Contains(output, "timed") || !(strings.Contains(output, "30m") || strings.Contains(output, "29m")) {
		t.Fatalf("the list must show the time that is left: status %d, output %q", status, output)
	}
}

func TestRunTunnelRejectsATimeLimitOutsideUp(t *testing.T) {
	for _, args := range [][]string{{"list", "--ttl", "30m"}, {"down", "--ttl", "30m", "db"}, {"up", "--ttl", "-5m", "db"}} {
		stderr := captureTraceStderr(t, func() {
			if status := runTunnel(args, "test"); status != 2 {
				t.Errorf("%v: status = %d, want 2", args, status)
			}
		})
		if !strings.Contains(stderr, "--ttl works only with edc tunnel up") {
			t.Errorf("%v: stderr = %q", args, stderr)
		}
	}
}

// 터미널이 아니면 이름 없는 up은 폼을 열지 않고 전과 같이 오류로 끝난다.
func TestRunTunnelUpWithoutANameStillFailsOutsideATerminal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", shortTunnelDir(t))
	stderr := captureTraceStderr(t, func() {
		if status := runTunnel([]string{"up"}, "test"); status != 2 {
			t.Errorf("status = %d, want 2", status)
		}
	})
	if !strings.Contains(stderr, "give a tunnel name") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestTunnelExpiryLockTellsWhetherTheTimerLives(t *testing.T) {
	store := tunnelStore{dir: t.TempDir()}
	record := tunnelRecord{Name: "db", Started: time.Unix(1700000000, 5)}
	if store.expiryRunning(record) {
		t.Fatal("no timer has started")
	}
	release, held, err := store.holdExpiry("db", record.Started.UnixNano())
	if err != nil || !held || !store.expiryRunning(record) {
		t.Fatalf("held %v, err %v", held, err)
	}
	// 두 번째 보조 프로세스는 잠금을 얻지 못해 바로 끝난다.
	if _, again, err := store.holdExpiry("db", record.Started.UnixNano()); again || err != nil {
		t.Fatalf("again %v, err %v", again, err)
	}
	// 같은 이름으로 다시 시작한 터널은 다른 잠금을 쓴다.
	if store.expiryRunning(tunnelRecord{Name: "db", Started: time.Unix(1700000001, 0)}) {
		t.Fatal("a new start must not see the old timer")
	}
	release()
	if store.expiryRunning(record) {
		t.Fatal("the timer ended")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(store.dir, "*"+tunnelExpiryLockSuffix)); len(leftovers) != 0 {
		t.Fatalf("leftovers = %v", leftovers)
	}
}
