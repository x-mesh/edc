//go:build !darwin && !linux

package edc

import "os"

type remoteFileOwner struct {
	uid, gid int
	ok       bool
}

func remoteOwnerOf(os.FileInfo) remoteFileOwner { return remoteFileOwner{} }
