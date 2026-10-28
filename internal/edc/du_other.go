//go:build !linux && !darwin

package edc

import (
	"context"
	"errors"
	"time"
)

const duDefaultWorkers = 4

func hostDuSources() (duSources, bool) { return duSources{}, false }

func duHostMounts() ([]diskMount, error) { return nil, errors.ErrUnsupported }

type duWalker struct{}

type duInode struct{}

func duWalk(context.Context, duOptions, duSources) (duScan, error) {
	return duScan{}, errors.ErrUnsupported
}

func duPoll(context.Context, *duWalker, *duNode, time.Time, uint64, map[*duNode]bool) duRefreshResult {
	return duRefreshResult{err: errors.ErrUnsupported}
}

func duRescan(context.Context, *duWalker, *duNode, string, bool, uint64) (duDirRescan, bool) {
	return duDirRescan{}, false
}

func duRescanFull(context.Context, *duWalker, *duNode, string, string, uint64) duRefreshResult {
	return duRefreshResult{err: errors.ErrUnsupported}
}

func (*duWalker) forget(map[*duNode]bool) []duInode { return nil }

func (*duWalker) orphans([]duInode) []*duNode { return nil }

func (*duWalker) reown(_, _ *duNode) {}

func (*duWalker) floor() uint64 { return 0 }
