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
		return runTunnelUp(context.Background(), environment, []string{"jump1"}, "once", adhoc, false)
	}); status != 0 {
		t.Fatalf("status = %d", status)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("up without --save must not write the file")
	}
	output, status := captureTunnelStdout(t, func() int {
		return runTunnelUp(context.Background(), environment, []string{"jump1"}, "kept", [3]string{"6001:db:5432", "", ""}, true)
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
	if status := runTunnelUp(context.Background(), environment, []string{"db"}, "", [3]string{}, true); status != 2 {
		t.Errorf("--save with a defined name = %d", status)
	}
	if status := runTunnelUp(context.Background(), environment, []string{"jump1"}, "db", [3]string{"6000:x:1", "", ""}, true); status != 2 {
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
