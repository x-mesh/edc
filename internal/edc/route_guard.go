package edc

import (
	"context"
	"fmt"
	"strings"
)

// routeRunner는 route 명령군이 외부 명령을 부르는 유일한 통로다. linux 구현은 system.go의
// commandOutput을 쓰고, 다른 OS는 오류만 돌려준다. 판정 쪽 코드는 이 인터페이스만 알아서 가짜 runner로
// darwin에서도 테스트할 수 있다.
type routeRunner interface {
	run(ctx context.Context, name string, args ...string) (string, error)
}

// 안전장치 사다리 등급. 1은 무장과 is-active 확인이 모두 확인된 상태다. 3과 4는 자기 차단 위험이
// 높을 때 --force 없이는 변경을 거부하는 기준이다.
const (
	guardTierArmedConfirmed   = 1
	guardTierArmedUnconfirmed = 3
	guardTierNoGuard          = 4
)

// systemdRunArgs는 transient timer 무장 argv를 만든다. AccuracySec=1s는 조건 없이 항상 붙인다.
// 기본 1분 coalescing 때문에 원복이 최대 1분 늦게 발화하는 것을 막는다.
func systemdRunArgs(unit string, seconds int, statePath, edcPath string) []string {
	return []string{
		"--collect",
		fmt.Sprintf("--on-active=%d", seconds),
		"--timer-property=AccuracySec=1s",
		"--unit=" + unit,
		edcPath, "route", "rollback", "--state", statePath,
	}
}

// routeRollbackUnitName은 실행마다 다른 유닛 이름을 만든다. --collect와 함께 쓰면 이름 재사용으로
// 인한 "Unit X.timer was already loaded" 실패가 없어진다.
func routeRollbackUnitName(runID string) string { return "edc-route-rollback-" + runID }

// parseIsActive는 systemctl is-active 출력을 판정한다. active만 참이고 activating, inactive,
// failed, 빈 문자열은 모두 거짓이다.
func parseIsActive(output string) bool {
	return strings.TrimSpace(output) == "active"
}

// guardProbe는 무장 시도와 is-active 확인의 실제 관측 결과다. detectGuardTier는 이 값만 보고 등급을
// 정하며, systemd-run과 systemctl 바이너리가 있다는 사실만으로는 등급을 올리지 않는다.
type guardProbe struct {
	ArmAttempted bool
	ArmSucceeded bool
	IsActive     bool
}

// detectGuardTier는 무장 시도와 is-active 확인이 모두 성공해야 1등급을 준다.
func detectGuardTier(probe guardProbe) int {
	if !probe.ArmAttempted || !probe.ArmSucceeded {
		return guardTierNoGuard
	}
	if !probe.IsActive {
		return guardTierArmedUnconfirmed
	}
	return guardTierArmedConfirmed
}

// guardDecision은 위험 등급과 안전장치 등급을 대조해 변경을 허용할지 정한다. 자기 차단 위험이 높고
// 등급이 3이나 4(무장이 확인되지 않음)면 --force 없이는 거부한다. force로 우회해도 사용한 등급을
// 사유에 남긴다.
func guardDecision(tier int, risk string, force bool) (allow bool, reason string) {
	if risk != "high" || (tier != guardTierArmedUnconfirmed && tier != guardTierNoGuard) {
		return true, T("route.guard.reason.allowed", tier, risk)
	}
	if force {
		return true, T("route.guard.reason.forced", tier, risk)
	}
	return false, T("route.guard.reason.denied", tier)
}

// armGuard는 systemd-run으로 타이머를 무장하고 즉시 is-active로 확인한다. systemd-run 자체가
// exit 0을 주고도 타이머가 없을 수 있어(R06) is-active 확인 없이는 무장으로 보지 않는다.
func armGuard(ctx context.Context, runner routeRunner, unit string, seconds int, statePath, edcPath string) guardProbe {
	args := systemdRunArgs(unit, seconds, statePath, edcPath)
	_, err := runner.run(ctx, "systemd-run", args...)
	probe := guardProbe{ArmAttempted: true, ArmSucceeded: err == nil}
	if !probe.ArmSucceeded {
		return probe
	}
	// systemctl is-active는 비활성 유닛에 0이 아닌 exit code를 주지만 출력("inactive" 등)은 여전히
	// 유효하므로 오류는 무시하고 출력만 판정에 쓴다.
	output, _ := runner.run(ctx, "systemctl", "is-active", unit+".timer")
	probe.IsActive = parseIsActive(output)
	return probe
}

// disarmGuard는 무장한 타이머를 해제한다. 이미 없는 유닛에 대한 실패는 경고 문자열로만 남기고 멈추지
// 않는다. rollback이 이미 실행된 상황에서는 유닛이 스스로 사라진 뒤일 수 있기 때문이다.
func disarmGuard(ctx context.Context, runner routeRunner, unit string) (warning string) {
	if _, err := runner.run(ctx, "systemctl", "stop", unit+".timer"); err != nil {
		return fmt.Sprintf("could not stop timer %s.timer: %v", unit, err)
	}
	return ""
}
