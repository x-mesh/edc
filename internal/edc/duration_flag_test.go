package edc

import (
	"bytes"
	"flag"
	"io"
	"strings"
	"testing"
	"time"
)

func TestParseFlagDurationTakesABareNumberAsSeconds(t *testing.T) {
	for input, want := range map[string]time.Duration{
		"1":     time.Second,
		"1s":    time.Second,
		"1m":    time.Minute,
		"1.5":   1500 * time.Millisecond,
		"0.1":   100 * time.Millisecond,
		"0":     0,
		"-2":    -2 * time.Second,
		"250ms": 250 * time.Millisecond,
		"1h30m": 90 * time.Minute,
	} {
		got, err := parseFlagDuration(input)
		if err != nil || got != want {
			t.Errorf("parseFlagDuration(%q) = %v, %v; want %v", input, got, err, want)
		}
	}
	for _, input := range []string{"", "abc", "1x", "NaN", "Inf", "1e300", "9223372036.854775807", "1000000000000000000000h"} {
		if got, err := parseFlagDuration(input); err == nil {
			t.Errorf("parseFlagDuration(%q) = %v, want an error", input, got)
		}
	}
}

func TestDurationFlagParsesBareSecondsAndKeepsTheDefault(t *testing.T) {
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	interval := durationFlag(set, "interval", time.Second, "")
	var duration time.Duration
	durationFlagVar(set, &duration, "duration", 0, "")
	if *interval != time.Second || duration != 0 {
		t.Fatalf("defaults = %v, %v", *interval, duration)
	}
	if err := set.Parse([]string{"--interval", "2", "--duration", "1m"}); err != nil {
		t.Fatal(err)
	}
	if *interval != 2*time.Second || duration != time.Minute {
		t.Fatalf("parsed = %v, %v", *interval, duration)
	}
	err := set.Parse([]string{"--interval", "soon"})
	if err == nil || !strings.Contains(err.Error(), `invalid value "soon" for flag -interval: `+errFlagDuration.Error()) {
		t.Fatalf("error = %v", err)
	}
}

func TestDurationFlagPrintsItsDefault(t *testing.T) {
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	var out bytes.Buffer
	set.SetOutput(&out)
	durationFlag(set, "timeout", 15*time.Second, "time limit")
	durationFlag(set, "duration", 0, "how long")
	set.PrintDefaults()
	if !strings.Contains(out.String(), "(default 15s)") || strings.Contains(out.String(), "(default 0s)") {
		t.Fatalf("defaults:\n%s", out.String())
	}
}
