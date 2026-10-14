//go:build !darwin && !linux

package edc

import "errors"

func newFSWatchSource(options fsWatchOptions) (fsWatchSource, error) {
	if options.driver != "" && options.driver != fsWatchDriverAuto {
		return fsWatchSource{}, errors.New(T("watchfs.driver_unsupported", options.driver))
	}
	return newFSNotifyWatchSource(options)
}
