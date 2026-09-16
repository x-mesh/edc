//go:build !linux

package edc

// newRouteBackend는 Linux가 아닌 OS에서는 backend를 주지 않는다. route.go는 ok가 false면 기존
// unsupported 관례로 건너뛴다.
func newRouteBackend() (routeBackend, bool) { return nil, false }
