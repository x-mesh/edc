//go:build linux

package edc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

func hostChangesSources() (changesSources, bool) {
	sources := changesSources{
		root: "/",
		now:  time.Now(),
		bootTime: func() (time.Time, error) {
			data, err := os.ReadFile("/proc/stat")
			if err != nil {
				return time.Time{}, err
			}
			return parseProcStatBtime(data)
		},
	}
	// /run/systemd/system은 systemd가 PID 1일 때만 있다. 컨테이너 안의 systemctl은 unit 상태를 주지 않는다.
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return sources, true
	}
	path, err := exec.LookPath("systemctl")
	if err != nil {
		return sources, true
	}
	sources.systemctl = func(ctx context.Context, args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, path, args...)
		// 속성 값은 숫자만 읽지만 오류 문장을 그대로 보이려고 locale을 고정한다.
		command.Env = append(os.Environ(), "LC_ALL=C")
		output, err := command.Output()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
				return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
			}
			return nil, err
		}
		return output, nil
	}
	return sources, true
}
