//go:build darwin

package edc

import (
	"runtime"
	"slices"
	"testing"
)

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

func TestAddDarwinCoreTicks(t *testing.T) {
	snapshot := resourceSnapshot{}
	addDarwinCoreTicks(&snapshot, []darwinCoreTicks{
		{User: 100, System: 50, Idle: 800, Nice: 10},
		{User: 300, System: 100, Idle: 600},
	})
	if snapshot.CPUUser != 410 || snapshot.CPUSystem != 150 || snapshot.CPUIdle != 1400 || snapshot.CPUTotal != 1960 {
		t.Fatalf("aggregate = %#v", snapshot)
	}
	if want := []resourceCPU{{Total: 960, Idle: 800}, {Total: 1000, Idle: 600}}; !slices.Equal(snapshot.Cores, want) {
		t.Fatalf("cores = %#v, want %#v", snapshot.Cores, want)
	}
}

func TestReadDarwinCoreTicks(t *testing.T) {
	first, err := readDarwinCoreTicks()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != runtime.NumCPU() {
		t.Fatalf("cores = %d, want %d", len(first), runtime.NumCPU())
	}
	// 반복 호출은 매번 결과 배열을 돌려준다. vm_deallocate가 실패하면 여기서 오류로 드러난다.
	var last []darwinCoreTicks
	for range 1000 {
		if last, err = readDarwinCoreTicks(); err != nil {
			t.Fatal(err)
		}
	}
	for index, core := range first {
		before := core.User + core.System + core.Idle + core.Nice
		after := last[index].User + last[index].System + last[index].Idle + last[index].Nice
		if after < before {
			t.Fatalf("core %d ticks went backwards: %d -> %d", index, before, after)
		}
	}
}
