//go:build !linux

package edc

import (
	"context"
	"errors"
)

// macOS는 APFS firmlink 때문에 장치 번호가 바뀌는 지점을 마운트로 보면 /Users 같은 경로가 빠진다.
// 그 처리를 확인할 호스트가 없어 Linux에서만 잰다.
func hostDuSources() (duSources, bool) { return duSources{}, false }

func duWalk(context.Context, duOptions, duSources) (duScan, error) {
	return duScan{}, errors.ErrUnsupported
}
