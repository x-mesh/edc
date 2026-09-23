package edc

import (
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"time"
)

const observeMinInterval = 100 * time.Millisecond

func parseObserveInterval(input string) (time.Duration, error) {
	interval, err := time.ParseDuration(input)
	if err != nil {
		seconds, parseErr := strconv.ParseFloat(input, 64)
		if parseErr != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds > float64(math.MaxInt64)/float64(time.Second) {
			return 0, fmt.Errorf("%s", T("observe.watch.interval_invalid"))
		}
		interval = time.Duration(math.Round(seconds * float64(time.Second)))
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
