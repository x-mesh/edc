//go:build linux

package edc

import (
	"os"
	"testing"
	"time"
)

func TestLinuxSwapCacheRereadsOnlyWhenStale(t *testing.T) {
	cache := &linuxSwapCache{}
	now := time.Unix(1_000_000, 0)
	reads := 0
	read := func(kb string) func() ([]byte, error) {
		return func() ([]byte, error) {
			reads++
			return []byte("Name:\tx\nVmSize:\t10 kB\nVmSwap:\t" + kb + " kB\n"), nil
		}
	}
	// PID 5는 5로 나눈 나머지가 0이라 처음 읽은 시각부터 간격을 센다.
	if swap, valid, gone := cache.get(5, 100, now, read("4")); swap != 4096 || !valid || gone || reads != 1 {
		t.Fatalf("first read: swap=%d valid=%v gone=%v reads=%d", swap, valid, gone, reads)
	}
	cache.sweep()
	if swap, _, _ := cache.get(5, 100, now.Add(linuxSwapRefresh-time.Second), read("8")); swap != 4096 || reads != 1 {
		t.Fatalf("inside the interval the cached value must stay: swap=%d reads=%d", swap, reads)
	}
	cache.sweep()
	if swap, _, _ := cache.get(5, 100, now.Add(linuxSwapRefresh), read("8")); swap != 8192 || reads != 2 {
		t.Fatalf("after the interval it must read again: swap=%d reads=%d", swap, reads)
	}
	cache.sweep()
	if swap, _, _ := cache.get(5, 200, now.Add(linuxSwapRefresh+time.Second), read("1")); swap != 1024 || reads != 3 {
		t.Fatalf("a reused PID must read at once: swap=%d reads=%d", swap, reads)
	}
	if _, _, gone := cache.get(6, 1, now, func() ([]byte, error) { return nil, os.ErrNotExist }); !gone {
		t.Fatal("a missing status file means the process is gone")
	}
	cache.sweep()
	cache.sweep()
	if len(cache.entries) != 0 {
		t.Fatalf("processes not seen in a read must be swept: %v", cache.entries)
	}
}
