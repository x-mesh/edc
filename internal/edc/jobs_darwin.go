//go:build darwin

package edc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

func hostJobsSources(ctx context.Context) []jobsSource {
	return []jobsSource{readLaunchdJobs(ctx)}
}

// readLaunchdJobs는 일정이 있는 launchd job을 읽는다. /System 아래는 macOS 자체의 job이라 뺀다. 사용자로 실행하면 launchctl
// list는 이 사용자의 agent만 알려 주므로, daemon의 결과는 sudo로 실행해야 보인다.
func readLaunchdJobs(ctx context.Context) jobsSource {
	source := jobsSource{Probe: jobsProbeLaunchd}
	directories := []string{"/Library/LaunchDaemons", "/Library/LaunchAgents"}
	if home, err := os.UserHomeDir(); err == nil {
		directories = append(directories, filepath.Join(home, "Library", "LaunchAgents"))
	}
	for _, directory := range directories {
		paths, _ := filepath.Glob(filepath.Join(directory, "*.plist"))
		for _, path := range paths {
			// plist는 binary일 수 있어 plutil로 XML로 바꿔 읽는다.
			output, _, err := runDarwinCheckTool(ctx, "/usr/bin/plutil", "-convert", "xml1", "-o", "-", path)
			if err != nil {
				if _, statErr := os.ReadFile(path); os.IsPermission(statErr) {
					source.Unreadable = append(source.Unreadable, path)
				}
				continue
			}
			value, err := parsePlist(strings.NewReader(output))
			if err != nil {
				continue
			}
			if plist, ok := value.(map[string]any); ok {
				if job, ok := launchdJob(path, plist); ok {
					source.Jobs = append(source.Jobs, job)
				}
			}
		}
	}
	output, _, err := runDarwinCheckTool(ctx, "/bin/launchctl", "list")
	if err != nil {
		source.Err = err
		return source
	}
	applyLaunchctlStatus(source.Jobs, output)
	return source
}
