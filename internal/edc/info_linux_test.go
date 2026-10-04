//go:build linux

package edc

import "testing"

func TestInfoMemoryFromLinuxRequiresAvailabilityAndKeepsCounterMeaning(t *testing.T) {
	values := map[string]uint64{"MemTotal": 1000, "MemAvailable": 400, "Cached": 200, "Buffers": 20, "Zswap": 0}
	memory, err := infoMemoryFromLinux(values)
	if err != nil || memory.Total-memory.Available != 600 || len(memory.Details) != 3 || memory.Details[0].Bytes != 200 || memory.Details[2].Bytes != 0 {
		t.Fatalf("memory = %+v, %v", memory, err)
	}
	for _, invalid := range []map[string]uint64{{"MemTotal": 1000}, {"MemAvailable": 400}, {"MemTotal": 1000, "MemAvailable": 1001}} {
		if _, err := infoMemoryFromLinux(invalid); err == nil {
			t.Fatalf("invalid memory counters accepted: %+v", invalid)
		}
	}
	if memory, err := infoMemoryFromLinux(map[string]uint64{"MemTotal": 1000, "MemAvailable": 0}); err != nil || memory.Available != 0 {
		t.Fatalf("zero available is valid: %+v, %v", memory, err)
	}
}
