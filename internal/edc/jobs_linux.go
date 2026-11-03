//go:build linux

package edc

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

func hostJobsSources(ctx context.Context) []jobsSource {
	return []jobsSource{readSystemdTimers(ctx), readCrontabs("/")}
}

// readSystemdTimers는 켜진 timer의 다음·마지막 실행과 그 timer가 깨우는 service의 마지막 결과를 읽는다. root가 필요 없다.
func readSystemdTimers(ctx context.Context) jobsSource {
	source := jobsSource{Probe: jobsProbeTimers}
	if !linuxSystemdRunning() {
		source.Err = errors.New(T("svc.skip.no_systemd"))
		return source
	}
	run := func(args ...string) (string, error) {
		output, err := exec.CommandContext(ctx, "systemctl", append([]string{"--no-pager"}, args...)...).Output()
		return string(output), err
	}
	listed, err := run("list-units", "--type=timer", "--state=active", "--plain", "--no-legend")
	if err != nil {
		source.Err = err
		return source
	}
	timers := parseTimerUnits(listed)
	if len(timers) == 0 {
		return source
	}
	show := func(properties string, units []string) ([]map[string]string, error) {
		output, err := run(append([]string{"show", "-p", properties, "--"}, units...)...)
		if err != nil {
			return nil, err
		}
		return parseSystemctlShowBlocks(output), nil
	}
	timerShows, err := show("Id,Unit,NextElapseUSecRealtime,LastTriggerUSec", timers)
	if err != nil {
		source.Err = err
		return source
	}
	var services []string
	for _, timer := range timerShows {
		if unit := timer["Unit"]; unit != "" && !strings.HasPrefix(unit, "-") {
			services = append(services, unit)
		}
	}
	var serviceShows []map[string]string
	if len(services) > 0 {
		if serviceShows, err = show("Id,Result,ExecMainStatus", services); err != nil {
			source.Err = err
			return source
		}
	}
	source.Jobs = timerJobs(timerShows, serviceShows, time.Local)
	return source
}
