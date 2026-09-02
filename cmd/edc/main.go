package main

import (
	"os"

	"github.com/x-mesh/edc/internal/edc"
)

var version = "dev"

func main() {
	os.Exit(edc.Run(os.Args[1:], version))
}
