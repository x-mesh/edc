package edc

import (
	"errors"
	"flag"
	"math"
	"strconv"
	"time"
)

var errFlagDuration = errors.New("use a number of seconds or a duration such as 500ms, 30s, or 1m")

// parseFlagDuration reads a Go duration and takes a number without a unit as
// seconds, so --interval 1 means 1s and --interval 1.5 means 1.5s.
func parseFlagDuration(text string) (time.Duration, error) {
	if duration, err := time.ParseDuration(text); err == nil {
		return duration, nil
	}
	seconds, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || math.Abs(seconds) >= float64(math.MaxInt64)/float64(time.Second) {
		return 0, errFlagDuration
	}
	return time.Duration(math.Round(seconds * float64(time.Second))), nil
}

type durationValue time.Duration

func (value *durationValue) Set(text string) error {
	duration, err := parseFlagDuration(text)
	if err != nil {
		return err
	}
	*value = durationValue(duration)
	return nil
}

func (value *durationValue) String() string { return (*time.Duration)(value).String() }

func durationFlagVar(set *flag.FlagSet, target *time.Duration, name string, value time.Duration, usage string) {
	*target = value
	set.Var((*durationValue)(target), name, usage)
}

func durationFlag(set *flag.FlagSet, name string, value time.Duration, usage string) *time.Duration {
	target := new(time.Duration)
	durationFlagVar(set, target, name, value, usage)
	return target
}
