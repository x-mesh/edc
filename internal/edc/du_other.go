//go:build !linux && !darwin

package edc

import (
	"context"
	"errors"
)

const duDefaultWorkers = 4

func hostDuSources() (duSources, bool) { return duSources{}, false }

func duHostMounts() ([]diskMount, error) { return nil, errors.ErrUnsupported }

func duWalk(context.Context, duOptions, duSources) (duScan, error) {
	return duScan{}, errors.ErrUnsupported
}
