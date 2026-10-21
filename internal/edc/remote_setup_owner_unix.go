//go:build darwin || linux

package edc

import (
	"os"
	"syscall"
)

type remoteFileOwner struct {
	uid, gid int
	ok       bool
}

func remoteOwnerOf(info os.FileInfo) remoteFileOwner {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return remoteFileOwner{}
	}
	return remoteFileOwner{uid: int(stat.Uid), gid: int(stat.Gid), ok: true}
}
