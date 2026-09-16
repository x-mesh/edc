//go:build linux

package edc

import "context"

// systemRouteRunner는 route 명령군이 ip, systemd-run, systemctl을 부르는 유일한 통로다.
// system.go의 commandOutput을 그대로 재사용해 출력 상한과 관찰자 처리를 다시 만들지 않는다.
type systemRouteRunner struct{}

func (systemRouteRunner) run(ctx context.Context, name string, args ...string) (string, error) {
	return commandOutput(ctx, name, args...)
}

// newRouteRunner는 이 OS에서 route 명령군을 쓸 수 있는지와 함께 runner를 돌려준다.
func newRouteRunner() (routeRunner, bool) { return systemRouteRunner{}, true }
