package edc

import (
	"io/fs"
	"syscall"
	"time"
)

func codecChangeTime(info fs.FileInfo) time.Time {
	stat := info.Sys().(*syscall.Stat_t)
	return time.Unix(stat.Ctimespec.Unix())
}
