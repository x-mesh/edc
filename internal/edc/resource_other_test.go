//go:build darwin

package edc

import "testing"

func TestParseDarwinAvailableMemory(t *testing.T) {
	output := `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                      100.
Pages active:                                   1000.
Pages inactive:                                  200.
Pages speculative:                                50.
Pages wired down:                                800.
Pages purgeable:                                  10.
`
	want := uint64(100+200+50) * 16384
	if got := parseDarwinAvailableMemory(output); got != want {
		t.Fatalf("available = %d, want %d", got, want)
	}
	if got := parseDarwinAvailableMemory("unexpected output"); got != 0 {
		t.Fatalf("unreadable output must return zero, got %d", got)
	}
}
