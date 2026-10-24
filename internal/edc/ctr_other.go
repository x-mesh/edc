//go:build !linux && !darwin

package edc

func hostCtrSources() (ctrSources, bool) { return ctrSources{}, false }
