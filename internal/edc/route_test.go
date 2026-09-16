package edc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// routeCheckFakeDeps는 실측 fixture를 백엔드 값으로 바꿔 주는 route check용 가짜 의존성이다.
// 커널 읽기는 백엔드가, systemd는 runner가 맡는다.
func routeCheckFakeDeps() (routeDeps, *fakeRouteRunner) {
	runner := &fakeRouteRunner{outputs: map[string]string{}}
	backend := &fakeRouteBackend{
		routeText:    routeTableFixture,
		ruleText:     ipRuleFixture,
		neighborText: neighborFixture,
		linkText:     linkFixture,
		getResults: map[string]string{
			"100.64.0.2": "100.64.0.2 via 10.20.1.1 dev enp1s0 src 10.20.1.5 uid 0",
			"100.64.0.1": "100.64.0.1 via 10.20.1.1 dev enp1s0 src 10.20.1.5 uid 0",
			"8.8.8.8":    "8.8.8.8 via 192.0.2.254 dev enp1s0 src 10.20.1.5 uid 0",
		},
	}
	return routeDeps{backend: backend, runner: runner}, runner
}

const routeCheckSSHConnection = "100.64.0.2 50015 100.64.0.1 22"

func TestRunRouteCheckResultsNeverMutates(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage(defaultLanguage)

	deps, runner := routeCheckFakeDeps()
	results := runRouteCheckResults(context.Background(), deps, routeDefaultDest, routeCheckSSHConnection, t.TempDir(), "")
	if len(results) != 5 {
		t.Fatalf("results = %d, want 5: %#v", len(results), results)
	}
	names := map[string]Result{}
	for _, result := range results {
		names[result.Probe] = result
	}
	for _, probe := range []string{routeProbeTable, routeProbeTarget, routeProbeLockout, routeProbeGuard, routeProbeReach} {
		if _, ok := names[probe]; !ok {
			t.Fatalf("missing probe %s in %#v", probe, results)
		}
	}
	for _, call := range runner.calls {
		if call[0] == "systemd-run" {
			t.Fatalf("check must never arm a timer: calls=%#v", runner.calls)
		}
		for _, token := range call {
			if token == "replace" || token == "del" {
				t.Fatalf("check must never mutate a route: calls=%#v", runner.calls)
			}
		}
	}
}

func TestRunRouteCheckResultsReportsRestDespiteMissingExits(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage(defaultLanguage)

	deps, _ := routeCheckFakeDeps()
	results := runRouteCheckResults(context.Background(), deps, routeDefaultDest, routeCheckSSHConnection, t.TempDir(), "")
	var target, table, lockout, guard Result
	for _, result := range results {
		switch result.Probe {
		case routeProbeTarget:
			target = result
		case routeProbeTable:
			table = result
		case routeProbeLockout:
			lockout = result
		case routeProbeGuard:
			guard = result
		}
	}
	foundExitsWarning := false
	for _, warning := range target.Warnings {
		if warning == T("route.target.warn.exits_missing") {
			foundExitsWarning = true
		}
	}
	if !foundExitsWarning {
		t.Fatalf("route.target must record a skip reason for the missing exits.yaml: %#v", target.Warnings)
	}
	// exits.yaml이 없어도 나머지 세 probe는 정상적으로 보고돼야 한다.
	if table.Status == "" || lockout.Status == "" || guard.Status == "" {
		t.Fatalf("the other probes must still report: table=%#v lockout=%#v guard=%#v", table, lockout, guard)
	}
	// routeTableFixture는 타입 접두사까지 모두 읽히므로 route.table은 pass를 보고한다. 파싱 경고 경로는
	// TestProbeRouteTableWarnsOnUnreadableLine이 따로 덮는다.
	if table.Status != StatusPass {
		t.Fatalf("table status = %v, want pass", table.Status)
	}
}

// 읽지 못한 줄은 조용히 버리지 않고 경고로 드러나야 한다. 버리면 경로 개수 불변식이 틀린 값을 쓴다.
func TestProbeRouteTableWarnsOnUnreadableLine(t *testing.T) {
	entries, parseErrors := parseRouteTable("default via 10.20.1.1 dev enp1s0 metric 100\n192.0.2.0/24 proto kernel scope link\n")
	if len(entries) != 1 {
		t.Fatalf("entries = %#v", entries)
	}
	if len(parseErrors) != 1 || !strings.Contains(parseErrors[0], "192.0.2.0/24") {
		t.Fatalf("parseErrors = %#v", parseErrors)
	}
}

func TestProbeRouteTargetExcludesMoreSpecificRoute(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage(defaultLanguage)

	deps, _ := routeCheckFakeDeps()
	result := probeRouteTarget(context.Background(), deps, routeDefaultDest, routeCheckSSHConnection, t.TempDir(), "")
	excluded, ok := result.Metrics["excluded"].([]string)
	if !ok || len(excluded) != 1 || !strings.HasPrefix(excluded[0], "8.8.8.8") {
		t.Fatalf("excluded = %#v, want exactly one entry starting with 8.8.8.8", result.Metrics["excluded"])
	}
	matched, ok := result.Metrics["matched"].([]string)
	if !ok || len(matched) != 2 {
		t.Fatalf("matched = %#v, want 2 entries", result.Metrics["matched"])
	}
	if result.Status != StatusWarn {
		t.Fatalf("status = %v, want warn", result.Status)
	}
}

func TestProbeRouteLockoutHighWhenSessionRidesTarget(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage(defaultLanguage)

	deps, _ := routeCheckFakeDeps()
	result := probeRouteLockout(context.Background(), deps, routeDefaultDest, routeCheckSSHConnection)
	if result.Status != StatusWarn {
		t.Fatalf("status = %v, want warn (risk high)", result.Status)
	}
	if risk, _ := result.Metrics["risk"].(string); risk != "high" {
		t.Fatalf("risk = %v, want high", result.Metrics["risk"])
	}
}

func TestProbeRouteLockoutSkipsWithoutSSHConnection(t *testing.T) {
	deps, _ := routeCheckFakeDeps()
	result := probeRouteLockout(context.Background(), deps, routeDefaultDest, "")
	if result.Status != StatusSkip {
		t.Fatalf("status = %v, want skip", result.Status)
	}
}

func TestProbeRouteGuardNeverArmsDuringCheck(t *testing.T) {
	deps, runner := routeCheckFakeDeps()
	result := probeRouteGuard(context.Background(), deps)
	if result.Status != StatusSkip {
		t.Fatalf("status = %v, want skip", result.Status)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("probeRouteGuard must not call the runner during check: %#v", runner.calls)
	}
}

func TestRunRouteCheckResultsUnsupportedWithoutBackend(t *testing.T) {
	results := runRouteCheckResults(context.Background(), routeDeps{}, routeDefaultDest, "", t.TempDir(), "")
	for _, result := range results {
		if result.Status != StatusSkip {
			t.Fatalf("probe %s status = %v, want skip when the OS is unsupported", result.Probe, result.Status)
		}
	}
}

// renderedRuleFixture는 프로덕션이 스냅샷에 저장하는 형태와 같은 렌더링 결과를 만든다.
func renderedRuleFixture() string {
	rules, _ := parseIPRule(ipRuleFixture)
	return renderIPRules(rules)
}

func metricTrapSnapshot() routeSnapshot {
	entries, _ := parseRouteTable(routeMetricTrapFixture)
	return routeSnapshot{
		SchemaVersion:  routeStateSchemaVersion,
		RunID:          "t1",
		CreatedAt:      time.Now().UTC(),
		Dest:           "8.8.8.8",
		RouteTableText: routeMetricTrapFixture,
		IPRuleText:     renderedRuleFixture(),
		TargetEntry:    entries[0],
		NewVia:         "192.0.2.254",
		CountBaseline:  1,
		UnitName:       "edc-route-rollback-t1",
		SSHConnection:  routeCheckSSHConnection,
		ExitName:       "lab-nat-02",
	}
}

func TestExecuteRouteRollbackRestoresAndDisarms(t *testing.T) {
	snapshot := metricTrapSnapshot()
	restoredTable := "8.8.8.8 via 10.20.1.1 dev enp1s0 metric 100\n"
	runner := &fakeRouteRunner{outputs: map[string]string{}}
	backend := &fakeRouteBackend{routeSequence: []string{routeMetricTrapFixture, restoredTable}, ruleText: ipRuleFixture}
	deps := routeDeps{backend: backend, runner: runner}
	statePath := filepath.Join(t.TempDir(), "route.json")
	outcome := executeRouteRollback(context.Background(), deps, statePath, snapshot)
	if outcome.Result.Status != StatusPass {
		t.Fatalf("outcome = %#v", outcome)
	}
	if len(outcome.RemainingCommands) != 0 {
		t.Fatalf("RemainingCommands = %#v, want none on success", outcome.RemainingCommands)
	}
	// 실행은 netlink이므로 runner가 아니라 백엔드가 받은 쓰기를 확인한다.
	if !backend.wroteDelete("8.8.8.8", "192.0.2.254") {
		t.Fatalf("expected the residual route to be deleted: writes=%#v", backend.writes)
	}
	loaded, err := readRouteState(statePath)
	if err != nil {
		t.Fatalf("readRouteState error: %v", err)
	}
	if loaded.CompletedAt == nil {
		t.Fatal("expected the state file to be marked complete")
	}
}

func TestExecuteRouteRollbackFailsWhenBaselineNotRestored(t *testing.T) {
	snapshot := metricTrapSnapshot()
	// del을 실행해도 여전히 두 줄을 돌려주는, 고쳐지지 않은 테이블을 흉내 낸다.
	backend := &fakeRouteBackend{routeSequence: []string{routeMetricTrapFixture, routeMetricTrapFixture}, ruleText: ipRuleFixture}
	deps := routeDeps{backend: backend, runner: &fakeRouteRunner{outputs: map[string]string{}}}
	statePath := filepath.Join(t.TempDir(), "route.json")
	outcome := executeRouteRollback(context.Background(), deps, statePath, snapshot)
	if outcome.Result.Status != StatusFail {
		t.Fatalf("outcome.Result.Status = %v, want fail", outcome.Result.Status)
	}
	if len(outcome.RemainingCommands) == 0 {
		t.Fatal("expected remaining commands for manual recovery when verification fails")
	}
	if _, err := readRouteState(statePath); err == nil {
		t.Fatal("a failed rollback must not write a completed state file")
	}
}

func TestRunRouteRollbackRejectsMissingStateFile(t *testing.T) {
	code := runRouteRollback([]string{"--state", filepath.Join(t.TempDir(), "absent.json")}, "test")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunRouteRollbackRejectsAlreadyCompletedState(t *testing.T) {
	snapshot := metricTrapSnapshot()
	completedAt := time.Now().UTC()
	snapshot.CompletedAt = &completedAt
	statePath := filepath.Join(t.TempDir(), "route.json")
	if err := writeRouteState(statePath, snapshot); err != nil {
		t.Fatalf("writeRouteState error: %v", err)
	}
	code := runRouteRollback([]string{"--state", statePath}, "test")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunRouteRollbackRequiresState(t *testing.T) {
	code := runRouteRollback(nil, "test")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

const routeSwitchTestExecID = "test1"

var routeSwitchTestExit = routeExit{Name: "lab-nat-02", Via: "192.0.2.254", Dev: "enp1s0", ExpectPublicIP: "203.0.113.20"}

// routeSwitchTestInput은 executeRouteSwitch 테스트가 공유하는 입력이다. statePath만 호출마다 새로
// 받는다.
func routeSwitchTestInput(statePath string) routeSwitchInput {
	return routeSwitchInput{
		dest: routeDefaultDest, exit: routeSwitchTestExit, seconds: routeDefaultRollbackSeconds,
		edcPath: "/usr/local/bin/edc", execID: routeSwitchTestExecID, statePath: statePath,
		sshConnection: routeCheckSSHConnection,
	}
}

// 프리플라이트는 확정 실패만 막는다. next-hop이 INCOMPLETE나 FAILED면 바꿔도 반드시 끊기므로
// 깨뜨린 뒤 되돌리는 대신 아무것도 만들지 않고 끝내야 한다.
func TestExecuteRouteSwitchRefusesUnreachableExit(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage(defaultLanguage)

	statePath := filepath.Join(t.TempDir(), "route.json")
	input := routeSwitchTestInput(statePath)
	input.exit = routeExit{Name: "dead", Via: "192.0.2.253", Dev: "enp1s0", ExpectPublicIP: "203.0.113.20"}
	runner := &fakeRouteRunner{outputs: map[string]string{}}
	backend := &fakeRouteBackend{routeText: routeTableFixture, ruleText: ipRuleFixture, neighborText: neighborFixture, linkText: linkFixture}
	outcome := executeRouteSwitch(context.Background(), routeSwitchDeps{routeDeps: routeDeps{backend: backend, runner: runner}, signals: make(chan os.Signal)}, input)
	if outcome.Result.Status != StatusFail {
		t.Fatalf("outcome = %#v, want fail", outcome.Result)
	}
	for _, call := range runner.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "replace") || strings.Contains(joined, "systemd-run") {
			t.Fatalf("an unreachable exit must not arm or mutate anything: %#v", runner.calls)
		}
	}
	if _, err := os.Stat(statePath); err == nil {
		t.Fatal("an unreachable exit must not leave a state file behind")
	}
}

// --force는 프리플라이트를 넘길 수 있어야 한다. 판정이 틀렸을 때 운영자가 막히면 안 된다.
func TestExecuteRouteSwitchForceOverridesUnreachableExit(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage(defaultLanguage)

	statePath := filepath.Join(t.TempDir(), "route.json")
	input := routeSwitchTestInput(statePath)
	input.exit = routeExit{Name: "dead", Via: "192.0.2.253", Dev: "enp1s0"}
	input.force = true
	input.dryRun = true // 실제 변경까지 가지 않고 프리플라이트를 통과했는지만 본다.
	runner := &fakeRouteRunner{outputs: map[string]string{}}
	backend := &fakeRouteBackend{routeText: routeTableFixture, ruleText: ipRuleFixture, neighborText: neighborFixture, linkText: linkFixture}
	outcome := executeRouteSwitch(context.Background(), routeSwitchDeps{routeDeps: routeDeps{backend: backend, runner: runner}, signals: make(chan os.Signal)}, input)
	if outcome.Result.Status != StatusPass {
		t.Fatalf("outcome = %#v, want pass with --force", outcome.Result)
	}
}

// dry-run은 계획만 보여주고 아무것도 바꾸지 않아야 한다. 위험한 명령일수록 먼저 볼 수 있어야 한다.
func TestExecuteRouteSwitchDryRunChangesNothing(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage(defaultLanguage)

	statePath := filepath.Join(t.TempDir(), "route.json")
	input := routeSwitchTestInput(statePath)
	input.dryRun = true
	runner := &fakeRouteRunner{outputs: map[string]string{}}
	backend := &fakeRouteBackend{routeText: routeTableFixture, ruleText: ipRuleFixture, neighborText: neighborFixture, linkText: linkFixture}
	outcome := executeRouteSwitch(context.Background(), routeSwitchDeps{routeDeps: routeDeps{backend: backend, runner: runner}, signals: make(chan os.Signal)}, input)
	if outcome.Result.Status != StatusPass {
		t.Fatalf("outcome = %#v, want pass", outcome.Result)
	}
	for _, call := range runner.calls {
		joined := strings.Join(call, " ")
		for _, forbidden := range []string{"replace", "del", "systemd-run", "systemctl"} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("dry-run must not run %q: %#v", forbidden, runner.calls)
			}
		}
	}
	if _, err := os.Stat(statePath); err == nil {
		t.Fatal("dry-run must not write a state file")
	}
	// 실행될 argv를 그대로 보여 줘야 계획으로서 쓸모가 있다.
	foundApply := false
	for _, evidence := range outcome.Result.Evidence {
		if strings.Contains(evidence.Value, "ip route replace default via 192.0.2.254 dev enp1s0 metric 100") {
			foundApply = true
		}
	}
	if !foundApply {
		t.Fatalf("dry-run must show the exact replace argv: %#v", outcome.Result.Evidence)
	}
}

func TestExecuteRouteSwitchSuccess(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage(defaultLanguage)

	statePath := filepath.Join(t.TempDir(), "route.json")
	input := routeSwitchTestInput(statePath)
	unit := routeRollbackUnitName(routeSwitchTestExecID)
	armArgs := systemdRunArgs(unit, input.seconds, input.statePath, input.edcPath)
	runner := &fakeRouteRunner{
		outputs: map[string]string{
			"systemctl is-active " + unit + ".timer": "active",
		},
		errors: map[string]error{
			"systemd-run " + strings.Join(armArgs, " "): nil,
		},
	}
	backend := &fakeRouteBackend{
		routeText: routeTableFixture, ruleText: ipRuleFixture,
		neighborText: neighborFixture, linkText: linkFixture,
		// 192.0.2.254를 타는 전용 경로라 원래 default entry(via 10.20.1.1)와 어긋난다 => risk low.
		getResults: map[string]string{"100.64.0.2": "100.64.0.2 via 192.0.2.254 dev enp1s0 src 10.20.1.5 uid 0"},
	}
	confirmed := false
	deps := routeSwitchDeps{
		routeDeps: routeDeps{backend: backend, runner: runner},
		confirm: func(detail, question string, initial bool) (bool, error) {
			confirmed = true
			return true, nil
		},
		fetchPublicIP: func(context.Context) (publicNetworkInfo, error) {
			return publicNetworkInfo{IP: routeSwitchTestExit.ExpectPublicIP}, nil
		},
		signals: make(chan os.Signal),
	}
	outcome := executeRouteSwitch(context.Background(), deps, input)
	if outcome.Cancelled || outcome.Result.Status != StatusPass {
		t.Fatalf("outcome = %#v", outcome)
	}
	if !confirmed {
		t.Fatal("expected the confirm function to be called")
	}
	if !backend.wroteReplace("default", "192.0.2.254") {
		t.Fatalf("expected a replace write: %#v", backend.writes)
	}
	loaded, err := readRouteState(statePath)
	if err != nil {
		t.Fatalf("readRouteState error: %v", err)
	}
	if loaded.CompletedAt == nil {
		t.Fatal("expected the state file to be marked complete")
	}
}

func TestExecuteRouteSwitchStopsBeforeReplaceWhenGuardUnconfirmed(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage(defaultLanguage)

	statePath := filepath.Join(t.TempDir(), "route.json")
	input := routeSwitchTestInput(statePath)
	unit := routeRollbackUnitName(routeSwitchTestExecID)
	runner := &fakeRouteRunner{
		outputs: map[string]string{
			"systemctl is-active " + unit + ".timer": "inactive",
		},
	}
	backend := &fakeRouteBackend{
		routeText: routeTableFixture, ruleText: ipRuleFixture, neighborText: neighborFixture, linkText: linkFixture,
		getResults: map[string]string{"100.64.0.2": "100.64.0.2 via 10.20.1.1 dev enp1s0 src 10.20.1.5 uid 0"},
	}
	deps := routeSwitchDeps{
		routeDeps: routeDeps{backend: backend, runner: runner},
		confirm:   func(string, string, bool) (bool, error) { t.Fatal("confirm must not be called"); return false, nil },
		fetchPublicIP: func(context.Context) (publicNetworkInfo, error) {
			t.Fatal("identity check must not run")
			return publicNetworkInfo{}, nil
		},
		signals: make(chan os.Signal),
	}
	outcome := executeRouteSwitch(context.Background(), deps, input)
	if outcome.Cancelled || outcome.Result.Status != StatusFail {
		t.Fatalf("outcome = %#v", outcome)
	}
	for _, call := range runner.calls {
		if len(call) >= 3 && call[0] == "ip" && call[1] == "route" && call[2] == "replace" {
			t.Fatalf("ip route replace must never be called: calls=%#v", runner.calls)
		}
	}
}

func TestExecuteRouteSwitchRollsBackWhenCountInvariantBreaks(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage(defaultLanguage)

	statePath := filepath.Join(t.TempDir(), "route.json")
	input := routeSwitchTestInput(statePath)
	unit := routeRollbackUnitName(routeSwitchTestExecID)
	armArgs := systemdRunArgs(unit, input.seconds, input.statePath, input.edcPath)
	// replace가 add로 동작해 목적지가 두 줄이 된 상태를 흉내 낸다. rollback이 복원한 뒤에는 한 줄로
	// 돌아온다.
	brokenTable := "default via 10.20.1.1 dev enp1s0 metric 100\ndefault via 192.0.2.254 dev enp1s0\n"
	restoredTable := "default via 10.20.1.1 dev enp1s0 metric 100\n"
	runner := &fakeRouteRunner{
		outputs: map[string]string{
			"systemctl is-active " + unit + ".timer": "active",
		},
		errors: map[string]error{
			"systemd-run " + strings.Join(armArgs, " "): nil,
		},
	}
	backend := &fakeRouteBackend{
		// 스냅샷 → 변경 후(두 줄) → rollback 후(한 줄) 순으로 읽힌다.
		routeSequence: []string{routeTableFixture, brokenTable, brokenTable, restoredTable},
		ruleText:      ipRuleFixture, neighborText: neighborFixture, linkText: linkFixture,
		getResults: map[string]string{"100.64.0.2": "100.64.0.2 via 192.0.2.254 dev enp1s0 src 10.20.1.5 uid 0"},
	}
	deps := routeSwitchDeps{
		routeDeps: routeDeps{backend: backend, runner: runner},
		confirm:   func(string, string, bool) (bool, error) { t.Fatal("confirm must not be called"); return false, nil },
		fetchPublicIP: func(context.Context) (publicNetworkInfo, error) {
			t.Fatal("identity check must not run")
			return publicNetworkInfo{}, nil
		},
		signals: make(chan os.Signal),
	}
	outcome := executeRouteSwitch(context.Background(), deps, input)
	if outcome.Cancelled || outcome.Result.Status != StatusFail {
		t.Fatalf("outcome = %#v", outcome)
	}
	if !backend.wroteDelete("default", "192.0.2.254") {
		t.Fatalf("expected the rollback path to delete the residual route: writes=%#v", backend.writes)
	}
}

func TestExecuteRouteSwitchRollsBackOnIdentityMismatch(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage(defaultLanguage)

	statePath := filepath.Join(t.TempDir(), "route.json")
	input := routeSwitchTestInput(statePath)
	unit := routeRollbackUnitName(routeSwitchTestExecID)
	armArgs := systemdRunArgs(unit, input.seconds, input.statePath, input.edcPath)
	runner := &fakeRouteRunner{
		outputs: map[string]string{
			"systemctl is-active " + unit + ".timer": "active",
		},
		errors: map[string]error{
			"systemd-run " + strings.Join(armArgs, " "): nil,
		},
	}
	backend := &fakeRouteBackend{
		routeText: routeTableFixture, ruleText: ipRuleFixture, neighborText: neighborFixture, linkText: linkFixture,
		getResults: map[string]string{"100.64.0.2": "100.64.0.2 via 192.0.2.254 dev enp1s0 src 10.20.1.5 uid 0"},
	}
	deps := routeSwitchDeps{
		routeDeps: routeDeps{backend: backend, runner: runner},
		confirm:   func(string, string, bool) (bool, error) { t.Fatal("confirm must not be called"); return false, nil },
		fetchPublicIP: func(context.Context) (publicNetworkInfo, error) {
			return publicNetworkInfo{IP: "198.51.100.9"}, nil // exit.ExpectPublicIP와 다르다.
		},
		signals: make(chan os.Signal),
	}
	outcome := executeRouteSwitch(context.Background(), deps, input)
	if outcome.Cancelled || outcome.Result.Status != StatusFail {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestExecuteRouteSwitchSkipsIdentityCheckWithoutExpectedIP(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage(defaultLanguage)

	statePath := filepath.Join(t.TempDir(), "route.json")
	input := routeSwitchTestInput(statePath)
	input.exit.ExpectPublicIP = ""
	unit := routeRollbackUnitName(routeSwitchTestExecID)
	armArgs := systemdRunArgs(unit, input.seconds, input.statePath, input.edcPath)
	runner := &fakeRouteRunner{
		outputs: map[string]string{
			"systemctl is-active " + unit + ".timer": "active",
		},
		errors: map[string]error{
			"systemd-run " + strings.Join(armArgs, " "): nil,
		},
	}
	backend := &fakeRouteBackend{
		routeText: routeTableFixture, ruleText: ipRuleFixture, neighborText: neighborFixture, linkText: linkFixture,
		getResults: map[string]string{"100.64.0.2": "100.64.0.2 via 192.0.2.254 dev enp1s0 src 10.20.1.5 uid 0"},
	}
	var seenInitial *bool
	deps := routeSwitchDeps{
		routeDeps: routeDeps{backend: backend, runner: runner},
		confirm: func(detail, question string, initial bool) (bool, error) {
			seenInitial = &initial
			return true, nil
		},
		fetchPublicIP: func(context.Context) (publicNetworkInfo, error) {
			t.Fatal("identity check must not run when expect_public_ip is empty")
			return publicNetworkInfo{}, nil
		},
		signals: make(chan os.Signal),
	}
	outcome := executeRouteSwitch(context.Background(), deps, input)
	if outcome.Cancelled || outcome.Result.Status != StatusPass {
		t.Fatalf("outcome = %#v", outcome)
	}
	if seenInitial == nil || *seenInitial != false {
		t.Fatalf("confirm initial = %v, want false (no automatic confirmation without an identity match)", seenInitial)
	}
}

func TestExecuteRouteSwitchRollsBackOnSignalBeforeConfirm(t *testing.T) {
	for _, signal := range []os.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM} {
		t.Run(signal.String(), func(t *testing.T) {
			restore := currentLanguage()
			defer setLanguage(restore)
			setLanguage(defaultLanguage)

			statePath := filepath.Join(t.TempDir(), "route.json")
			input := routeSwitchTestInput(statePath)
			unit := routeRollbackUnitName(routeSwitchTestExecID)
			armArgs := systemdRunArgs(unit, input.seconds, input.statePath, input.edcPath)
			runner := &fakeRouteRunner{
				outputs: map[string]string{
					"systemctl is-active " + unit + ".timer": "active",
				},
				errors: map[string]error{
					"systemd-run " + strings.Join(armArgs, " "): nil,
				},
			}
			backend := &fakeRouteBackend{
				routeText: routeTableFixture, ruleText: ipRuleFixture, neighborText: neighborFixture, linkText: linkFixture,
				getResults: map[string]string{"100.64.0.2": "100.64.0.2 via 192.0.2.254 dev enp1s0 src 10.20.1.5 uid 0"},
			}
			signals := make(chan os.Signal, 1)
			signals <- signal
			never := make(chan struct{})
			deps := routeSwitchDeps{
				routeDeps: routeDeps{backend: backend, runner: runner},
				confirm: func(string, string, bool) (bool, error) {
					<-never // 확정 대기를 흉내 낸다: 신호가 먼저 도착해야 한다.
					return true, nil
				},
				fetchPublicIP: func(context.Context) (publicNetworkInfo, error) {
					return publicNetworkInfo{IP: routeSwitchTestExit.ExpectPublicIP}, nil
				},
				signals: signals,
			}
			outcome := executeRouteSwitch(context.Background(), deps, input)
			if !outcome.Cancelled {
				t.Fatalf("outcome = %#v, want Cancelled=true", outcome)
			}
			if !backend.wroteReplace("default", "10.20.1.1") {
				t.Fatalf("expected the rollback restore write: writes=%#v", backend.writes)
			}
		})
	}
}

func TestRunRouteSwitchRejectsSecondsOutOfRange(t *testing.T) {
	for _, seconds := range []string{"5", "901"} {
		code := runRouteSwitch([]string{"--to", "lab-nat-02", "--seconds", seconds}, "test")
		if code != 2 {
			t.Fatalf("--seconds %s: exit code = %d, want 2", seconds, code)
		}
	}
}

func TestRunRouteSwitchRequiresTo(t *testing.T) {
	code := runRouteSwitch(nil, "test")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func writeRouteExitsFixtureFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "exits.yaml")
	if err := os.WriteFile(path, []byte(routeExitsFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunRouteSwitchRejectsNonRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this test process runs as root")
	}
	exitsPath := writeRouteExitsFixtureFile(t)
	code := runRouteSwitch([]string{"--to", "lab-nat-02", "--exits", exitsPath}, "test")
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
}

func TestRunRouteSwitchRejectsUnknownExitBeforeAnyCommand(t *testing.T) {
	exitsPath := writeRouteExitsFixtureFile(t)
	code := runRouteSwitch([]string{"--to", "lab-nat-99", "--exits", exitsPath}, "test")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRouteStatusResultsSkipsWhenDirectoryMissing(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage(defaultLanguage)

	results := routeStatusResults(context.Background(), routeDeps{runner: &fakeRouteRunner{}}, filepath.Join(t.TempDir(), "absent"))
	if len(results) != 1 || results[0].Status != StatusSkip {
		t.Fatalf("results = %#v, want a single skip result", results)
	}
}

func TestRouteStatusResultsReportsPendingAndCompleted(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage(defaultLanguage)

	dir := t.TempDir()
	pending := metricTrapSnapshot()
	pending.RunID = "pend1"
	pending.UnitName = "edc-route-rollback-pend1"
	pending.Seconds = 120
	if err := writeRouteState(filepath.Join(dir, "route-pend1.json"), pending); err != nil {
		t.Fatal(err)
	}
	completedAt := time.Now().UTC()
	done := metricTrapSnapshot()
	done.RunID = "done1"
	done.UnitName = "edc-route-rollback-done1"
	done.CompletedAt = &completedAt
	if err := writeRouteState(filepath.Join(dir, "route-done1.json"), done); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRouteRunner{
		outputs: map[string]string{
			"systemctl is-active edc-route-rollback-pend1.timer": "active",
		},
	}
	results := routeStatusResults(context.Background(), routeDeps{runner: runner}, dir)
	if len(results) != 2 {
		t.Fatalf("results = %#v, want 2", results)
	}
	var pendingResult, completedResult Result
	for _, result := range results {
		switch result.Probe {
		case "route.status.pend1":
			pendingResult = result
		case "route.status.done1":
			completedResult = result
		}
	}
	if pendingResult.Status != StatusPass {
		t.Fatalf("pending result = %#v, want pass (timer active)", pendingResult)
	}
	if completed, _ := completedResult.Metrics["completed"].(bool); !completed {
		t.Fatalf("completed result = %#v, want completed=true", completedResult)
	}
}

func TestRouteStatusForFileWarnsOnTimerMismatch(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage(defaultLanguage)

	dir := t.TempDir()
	pending := metricTrapSnapshot()
	pending.RunID = "mismatch1"
	pending.UnitName = "edc-route-rollback-mismatch1"
	path := filepath.Join(dir, "route-mismatch1.json")
	if err := writeRouteState(path, pending); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRouteRunner{
		outputs: map[string]string{
			"systemctl is-active edc-route-rollback-mismatch1.timer": "inactive",
		},
	}
	result := routeStatusForFile(context.Background(), routeDeps{runner: runner}, path)
	if result.Status != StatusWarn {
		t.Fatalf("status = %v, want warn", result.Status)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("expected a warning describing the timer/state mismatch")
	}
}

// 정책 라우팅 변화는 경로 복원 성공과 별개다. Tailscale처럼 ip rule을 관리하는 소프트웨어는 연결
// 상태가 바뀌면 rule을 재구성하는데, 경로 전환이 바로 그 연결 상태를 바꾼다. 실패로 보면 경로는
// 제대로 돌아왔는데 실패를 보고하는 오경보가 난다.
func TestExecuteRouteRollbackWarnsButSucceedsWhenOnlyRulesChanged(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage(defaultLanguage)

	snapshot := metricTrapSnapshot()
	restoredTable := "8.8.8.8 via 10.20.1.1 dev enp1s0 metric 100\n"
	// 롤백 뒤 읽는 rule이 스냅샷과 다르다. 경로 개수는 기준선과 같다.
	changedRules := ipRuleFixture + "9000:\tfrom all lookup 99\n"
	runner := &fakeRouteRunner{outputs: map[string]string{}}
	backend := &fakeRouteBackend{
		routeSequence: []string{routeMetricTrapFixture, restoredTable},
		ruleText:      changedRules,
	}
	deps := routeDeps{backend: backend, runner: runner}
	outcome := executeRouteRollback(context.Background(), deps, filepath.Join(t.TempDir(), "route.json"), snapshot)
	if outcome.Result.Status != StatusWarn {
		t.Fatalf("status = %v, want warn: %#v", outcome.Result.Status, outcome.Result)
	}
	if len(outcome.RemainingCommands) != 0 {
		t.Fatalf("rule 변화만으로 수동 복구 명령을 남기면 안 된다: %#v", outcome.RemainingCommands)
	}
	found := false
	for _, warning := range outcome.Result.Warnings {
		if warning == T("route.rollback.warn.rules_changed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("rule 변화를 경고로 알려야 한다: %#v", outcome.Result.Warnings)
	}
}

// 경로 개수 불변식이 깨지면 여전히 실패다. 메시지도 개수를 말하므로 이제 내용이 맞는다.
func TestExecuteRouteRollbackStillFailsOnCountInvariant(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage(defaultLanguage)

	snapshot := metricTrapSnapshot()
	runner := &fakeRouteRunner{outputs: map[string]string{}}
	backend := &fakeRouteBackend{
		// 롤백 뒤에도 두 줄이 남는다.
		routeSequence: []string{routeMetricTrapFixture, routeMetricTrapFixture},
		ruleText:      ipRuleFixture,
	}
	deps := routeDeps{backend: backend, runner: runner}
	outcome := executeRouteRollback(context.Background(), deps, filepath.Join(t.TempDir(), "route.json"), snapshot)
	if outcome.Result.Status != StatusFail {
		t.Fatalf("status = %v, want fail", outcome.Result.Status)
	}
	if len(outcome.RemainingCommands) == 0 {
		t.Fatal("개수 불변식 실패에는 수동 복구 명령을 남겨야 한다")
	}
}
