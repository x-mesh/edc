//go:build linux

package edc

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
)

type systemChangeRunner struct{}

func (systemChangeRunner) run(ctx context.Context, name string, args ...string) (string, error) {
	return commandOutput(ctx, name, args...)
}

func (systemChangeRunner) runInput(ctx context.Context, input []byte, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if len(output) > probeOutputLimit {
		output = output[:probeOutputLimit]
	}
	return strings.TrimSpace(string(output)), err
}

func newChangeRunner() (changeRunner, bool) { return systemChangeRunner{}, true }
