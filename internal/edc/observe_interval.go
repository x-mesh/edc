package edc

import (
	"fmt"
	"io"
	"os"
	"time"
)

const observeMinInterval = 100 * time.Millisecond

func parseObserveInterval(input string) (time.Duration, error) {
	interval, err := parseFlagDuration(input)
	if err != nil {
		return 0, fmt.Errorf("%s", T("observe.watch.interval_invalid"))
	}
	if interval < observeMinInterval {
		return 0, fmt.Errorf("%s", T("observe.watch.interval_minimum", observeMinInterval))
	}
	return interval, nil
}

func openObserveStream(path string) (io.Writer, func() error, error) {
	if path == "" || path == "-" {
		return os.Stdout, func() error { return nil }, nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, nil, err
	}
	return file, file.Close, nil
}
