//go:build linux

package main

import (
	"io"
	"os"

	"github.com/x-mesh/edc/internal/edc"
)

func main() {
	input, err := io.ReadAll(io.LimitReader(os.Stdin, (64<<10)+1))
	if err != nil || len(input) > 64<<10 {
		os.Exit(2)
	}
	output, code := edc.RunCtrCollector(input)
	if code != 0 {
		os.Exit(code)
	}
	if _, err = os.Stdout.Write(output); err != nil {
		os.Exit(1)
	}
}
