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

func TestParseDarwinCPUReadsUsageLine(t *testing.T) {
	output := "Processes: 712 total, 3 running\n" +
		"CPU usage: 8.23% user, 14.53% sys, 77.22% idle \n" +
		"Load Avg: 5.88, 6.92, 7.63\n"
	sample, ok := parseDarwinCPU(output)
	if !ok || !sample.valid {
		t.Fatalf("usage line must parse: %#v", sample)
	}
	if sample.user != 823 || sample.system != 1453 || sample.idle != 7722 {
		t.Fatalf("sample = %#v", sample)
	}
	if sample.total != 10000 {
		t.Fatalf("total = %d, want 10000 for a percentage scale", sample.total)
	}
}

func TestParseDarwinCPUWithoutUsageLine(t *testing.T) {
	if _, ok := parseDarwinCPU("Load Avg: 1.0, 1.0, 1.0\n"); ok {
		t.Fatal("output without a CPU usage line must fail")
	}
}
