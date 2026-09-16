package edc

import "context"

// routeBackend는 커널 라우팅 상태를 읽는 통로다. ip 명령의 사람용 출력을 해석하는 대신 커널에서
// 타입 있는 값을 받는다. 이 프로젝트에서 발견된 route 결함은 전부 텍스트 전사 오류였고, 이 경계가
// 그 종류의 버그를 구조적으로 없앤다.
//
// systemd-run과 systemctl은 netlink 영역이 아니므로 commandRunner로 남는다. 두 인터페이스를 나눠
// 각각이 무엇을 할 수 있는지 타입으로 드러낸다.
type routeBackend interface {
	// Routes는 모든 테이블의 경로를 돌려준다. main 테이블만 보면 정책 라우팅을 쓰는 호스트에서
	// 스냅샷이 실제 상태를 놓친다.
	Routes(ctx context.Context) ([]routeEntry, error)
	// RouteTo는 목적지 하나가 실제로 어느 경로를 타는지 커널에 묻는다.
	RouteTo(ctx context.Context, address string) (routeGetResult, error)
	Rules(ctx context.Context) ([]ipRule, error)
	Neighbors(ctx context.Context) ([]neighborEntry, error)
	Links(ctx context.Context) ([]linkInfo, error)
}

// routeDeps는 route 명령이 바깥 세계와 닿는 두 통로다. 커널 상태는 netlink으로 읽고, systemd는
// routeRunner로 부른다. systemd는 netlink 영역이 아니므로 명령으로 남는다.
type routeDeps struct {
	backend routeBackend
	runner  routeRunner
}

// newRouteDeps는 이 OS에서 route 명령을 쓸 수 있는지와 함께 두 통로를 돌려준다.
func newRouteDeps() (routeDeps, bool) {
	backend, backendOK := newRouteBackend()
	runner, runnerOK := newRouteRunner()
	return routeDeps{backend: backend, runner: runner}, backendOK && runnerOK
}
