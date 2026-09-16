//go:build !linux

package edc

// newRouteRunner는 Linux가 아닌 OS에서는 runner를 주지 않는다. route.go는 ok가 false면 기존
// unsupported(probe, unsupportedOSReason()) 관례로 건너뛴다.
func newRouteRunner() (routeRunner, bool) { return nil, false }
