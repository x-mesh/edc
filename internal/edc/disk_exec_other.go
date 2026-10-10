//go:build !linux

package edc

func newDiskSystem() (diskSystem, bool) { return diskSystem{}, false }
