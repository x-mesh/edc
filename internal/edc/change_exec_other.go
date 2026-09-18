//go:build !linux

package edc

func newChangeRunner() (changeRunner, bool) { return nil, false }
