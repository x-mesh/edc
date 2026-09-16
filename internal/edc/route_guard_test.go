package edc

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRouteRunner는 route_exec_linux.go의 실제 실행 없이 route 명령군을 테스트하는 가짜 runner다.
// sequences에 값이 있으면 같은 key를 부를 때마다 앞에서부터 하나씩 꺼내 쓴다. rollback처럼 같은
// 명령(`ip route show table all`)을 실행 전후로 두 번 불러 서로 다른 결과를 봐야 하는 테스트에 쓴다.
type fakeRouteRunner struct {
	outputs   map[string]string
	errors    map[string]error
	sequences map[string][]string
	calls     [][]string
}

func (runner *fakeRouteRunner) key(name string, args []string) string {
	return name + " " + strings.Join(args, " ")
}

func (runner *fakeRouteRunner) run(_ context.Context, name string, args ...string) (string, error) {
	call := append([]string{name}, args...)
	runner.calls = append(runner.calls, call)
	key := runner.key(name, args)
	if queue, ok := runner.sequences[key]; ok && len(queue) > 0 {
		runner.sequences[key] = queue[1:]
		return queue[0], nil
	}
	return runner.outputs[key], runner.errors[key]
}

func TestSystemdRunArgsOrderAndContent(t *testing.T) {
	args := systemdRunArgs("edc-route-rollback-abc123", 120, "/run/edc/route-abc123.json", "/usr/local/bin/edc")
	want := []string{
		"--collect",
		"--on-active=120",
		"--timer-property=AccuracySec=1s",
		"--unit=edc-route-rollback-abc123",
		"/usr/local/bin/edc", "route", "rollback", "--state", "/run/edc/route-abc123.json",
	}
	if len(args) != len(want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
	for index := range want {
		if args[index] != want[index] {
			t.Fatalf("args[%d] = %q, want %q (full: %#v)", index, args[index], want[index], args)
		}
	}
}

func TestRouteRollbackUnitNameDiffersPerRun(t *testing.T) {
	first := routeRollbackUnitName("abc123")
	second := routeRollbackUnitName("def456")
	if first == second {
		t.Fatalf("unit names must differ per run: %q == %q", first, second)
	}
	if !strings.HasPrefix(first, "edc-route-rollback-") {
		t.Fatalf("unit name = %q, want edc-route-rollback- prefix", first)
	}
}

func TestParseIsActive(t *testing.T) {
	cases := map[string]bool{
		"active": true, "active\n": true,
		"activating": false, "inactive": false, "failed": false, "": false,
	}
	for input, want := range cases {
		if got := parseIsActive(input); got != want {
			t.Fatalf("parseIsActive(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestDetectGuardTierRequiresArmAndConfirmation(t *testing.T) {
	cases := []struct {
		name  string
		probe guardProbe
		want  int
	}{
		{"confirmed armed", guardProbe{ArmAttempted: true, ArmSucceeded: true, IsActive: true}, guardTierArmedConfirmed},
		{"armed but not active", guardProbe{ArmAttempted: true, ArmSucceeded: true, IsActive: false}, guardTierArmedUnconfirmed},
		{"arm attempt failed", guardProbe{ArmAttempted: true, ArmSucceeded: false}, guardTierNoGuard},
		{"never attempted", guardProbe{}, guardTierNoGuard},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := detectGuardTier(testCase.probe); got != testCase.want {
				t.Fatalf("detectGuardTier(%#v) = %d, want %d", testCase.probe, got, testCase.want)
			}
		})
	}
}

func TestGuardDecisionTable(t *testing.T) {
	cases := []struct {
		tier  int
		risk  string
		force bool
		allow bool
	}{
		{guardTierArmedConfirmed, "high", false, true},
		{guardTierArmedConfirmed, "high", true, true},
		{guardTierArmedUnconfirmed, "high", false, false},
		{guardTierNoGuard, "high", false, false},
		{guardTierArmedUnconfirmed, "high", true, true},
		{guardTierNoGuard, "high", true, true},
		{guardTierNoGuard, "low", false, true},
	}
	for _, testCase := range cases {
		allow, reason := guardDecision(testCase.tier, testCase.risk, testCase.force)
		if allow != testCase.allow || reason == "" {
			t.Fatalf("guardDecision(%d, %q, %v) = (%v, %q), want allow=%v", testCase.tier, testCase.risk, testCase.force, allow, reason, testCase.allow)
		}
	}
}

func TestArmGuardFailsOnAlreadyLoadedUnit(t *testing.T) {
	unit := "edc-route-rollback-dup"
	args := systemdRunArgs(unit, 120, "/run/edc/route-dup.json", "/usr/local/bin/edc")
	runner := &fakeRouteRunner{
		errors: map[string]error{
			"systemd-run " + strings.Join(args, " "): errors.New("Unit edc-route-rollback-dup.timer was already loaded"),
		},
	}
	probe := armGuard(context.Background(), runner, unit, 120, "/run/edc/route-dup.json", "/usr/local/bin/edc")
	if probe.ArmSucceeded {
		t.Fatalf("probe = %#v, want ArmSucceeded=false", probe)
	}
	for _, call := range runner.calls {
		if call[0] == "systemctl" {
			t.Fatalf("is-active must not run after a failed arm: calls=%#v", runner.calls)
		}
	}
}

func TestArmGuardConfirmsIsActive(t *testing.T) {
	unit := "edc-route-rollback-ok"
	args := systemdRunArgs(unit, 120, "/run/edc/route-ok.json", "/usr/local/bin/edc")
	runner := &fakeRouteRunner{
		outputs: map[string]string{
			"systemctl is-active " + unit + ".timer": "active",
		},
		errors: map[string]error{
			"systemd-run " + strings.Join(args, " "): nil,
		},
	}
	probe := armGuard(context.Background(), runner, unit, 120, "/run/edc/route-ok.json", "/usr/local/bin/edc")
	if !probe.ArmSucceeded || !probe.IsActive {
		t.Fatalf("probe = %#v, want fully armed and active", probe)
	}
}

func TestDisarmGuardWarnsOnMissingUnit(t *testing.T) {
	runner := &fakeRouteRunner{
		errors: map[string]error{
			"systemctl stop edc-route-rollback-gone.timer": errors.New("Unit edc-route-rollback-gone.timer not loaded"),
		},
	}
	warning := disarmGuard(context.Background(), runner, "edc-route-rollback-gone")
	if warning == "" {
		t.Fatal("expected a warning when the unit is already gone")
	}
}
