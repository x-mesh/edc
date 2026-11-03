//go:build !linux && !darwin

package edc

import "context"

func hostJobsSources(context.Context) []jobsSource {
	return []jobsSource{{Probe: jobsProbeTimers, Err: errJobsUnsupported}}
}
