//go:build !darwin

package edc

func resolveTraceHTTPTLSTargets(mode traceTLSMode, implicit bool, _ string) (*traceTLSFinder, []string, int, error) {
	return resolveTraceTLSTargetsFor(mode, implicit)
}
