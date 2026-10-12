//go:build linux

package edc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// journalctl은 이 문구를 번역하지 않고, LC_ALL=C로 남은 위험도 없앤다.
// 권한이 없으면 자기 기록만 보여 주고 0으로 끝나므로, 문구로 알아내지 않으면 빈 결과가 통과로 보인다.
var eventsJournalPermissionHints = []string{
	"insufficient permissions",
	"not seeing messages from other users",
}

func hostEventsSources() (eventsSources, bool) {
	now := time.Now()
	var monotonic unix.Timespec
	bootErr := unix.ClockGettime(unix.CLOCK_MONOTONIC, &monotonic)
	return eventsSources{
		now: now,
		// CLOCK_MONOTONIC은 suspend 동안 멈추므로, suspend한 호스트에서는 /dev/kmsg 시각이 실제보다 늦게 보인다.
		boot: now.Add(-time.Duration(monotonic.Nano())),
		kmsg: func(ctx context.Context) ([]string, error) {
			if bootErr != nil {
				return nil, fmt.Errorf("clock_gettime: %w", bootErr)
			}
			return readHostKmsg(ctx)
		},
		journal: readHostJournal,
	}, true
}

func readHostKmsg(ctx context.Context) ([]string, error) {
	fd, err := unix.Open("/dev/kmsg", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		// kernel.dmesg_restrict=1이면 root가 아닌 사용자는 열지 못한다.
		return nil, errEventsNeedsRoot
	}
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	return readKmsgRecords(ctx, func(buffer []byte) (int, error) { return unix.Read(fd, buffer) })
}

func readHostJournal(ctx context.Context, start time.Time) (io.Reader, []string, error) {
	path, err := exec.LookPath("journalctl")
	if err != nil {
		return nil, nil, errEventsNoJournal
	}
	command := exec.CommandContext(ctx, path, "-o", "json", "--no-pager", "--since", fmt.Sprintf("@%d", start.Unix()),
		"-n", fmt.Sprint(eventsJournalMaxEntries), "_PID=1", "+", "_TRANSPORT=kernel")
	command.Env = append(os.Environ(), "LC_ALL=C")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := command.Start(); err != nil {
		return nil, nil, err
	}
	// 상한보다 1바이트 더 읽어 넘쳤는지를 readJournalEntries가 알 수 있게 한다.
	data, readErr := io.ReadAll(io.LimitReader(stdout, eventsJournalLimit+1))
	overflow := len(data) > eventsJournalLimit
	if overflow {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	for _, hint := range eventsJournalPermissionHints {
		if strings.Contains(stderr.String(), hint) {
			return nil, nil, errEventsNeedsRoot
		}
	}
	if ctx.Err() != nil {
		return nil, nil, fmt.Errorf("journalctl: %w", ctx.Err())
	}
	if readErr != nil {
		return nil, nil, fmt.Errorf("journalctl: %w", readErr)
	}
	if waitErr != nil && !overflow {
		return nil, nil, fmt.Errorf("journalctl: %w: %s", waitErr, firstLine(strings.TrimSpace(stderr.String())))
	}
	return bytes.NewReader(data), nil, nil
}
