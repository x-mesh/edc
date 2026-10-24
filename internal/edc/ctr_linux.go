//go:build linux

package edc

import (
	"os"
	"path/filepath"
	"strconv"
)

func linuxCtrCgroupPath(pid int) (string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", err
	}
	return linuxUnifiedCgroupPath(string(data))
}
