//go:build linux

package edc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadLinuxCgroupPids(t *testing.T) {
	for _, tc := range []struct {
		name      string
		files     map[string]string
		want      ctrPidsLimits
		anyReason bool
	}{
		{"limited", map[string]string{"pids.current": "12\n", "pids.max": "512\n"}, ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "available"}, Current: 12, Max: 512}, false},
		{"unlimited", map[string]string{"pids.current": "3\n", "pids.max": "max\n"}, ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "available"}, Current: 3, Unlimited: true}, false},
		{"invalid", map[string]string{"pids.current": "many\n", "pids.max": "max\n"}, ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "error", Reason: "invalid pids.current"}}, false},
		// pids controller를 켜지 않은 cgroup에는 파일이 없다. 오류가 아니라 지원하지 않음이다.
		{"no controller", nil, ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "unsupported"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			for name, data := range tc.files {
				if err := os.WriteFile(filepath.Join(directory, name), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got := readLinuxCgroupPids(directory)
			if tc.anyReason {
				if got.Status != tc.want.Status || got.Reason == "" {
					t.Fatalf("pids = %+v", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("pids = %+v, want %+v", got, tc.want)
			}
		})
	}
}
