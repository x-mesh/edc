//go:build !linux

package edc

func collectNetworkHealth() *networkHealth {
	return &networkHealth{Scope: "Linux only"}
}
