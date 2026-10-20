//go:build darwin

package edc

import (
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// macOS reports ru_maxrss in bytes.
func rusageMaxRSSBytes(usage *syscall.Rusage) uint64 {
	if usage.Maxrss < 0 {
		return 0
	}
	return uint64(usage.Maxrss)
}

// newLogResourceProbe measures the process group on macOS. Docker Desktop runs
// containers in a VM, so docker run and docker exec stay group measurements.
func newLogResourceProbe(_ []string, pid int, started time.Time) logResourceProbe {
	totals := &logMemberTotals{}
	timebase, err := darwinMachTimebase()
	// Under Rosetta proc_pid_rusage reports kernel ticks that the translated
	// timebase does not describe, so CPU time is left unknown there.
	cpuValid := err == nil && !darwinTranslated()
	return &logGroupProbe{
		group: newLogResourceAggregate(started, true),
		readGroup: func() (logResourceReading, bool) {
			processes, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pid)
			if err != nil {
				return logResourceReading{}, false
			}
			members := map[logProcessMember]logResourceReading{}
			for _, process := range processes {
				member := int(process.Proc.P_pid)
				usage, err := readDarwinProcessRusage(member)
				if err != nil {
					continue
				}
				reading := logResourceReading{memory: usage.Footprint, diskRead: usage.DiskRead, diskWrite: usage.DiskWrite, cpuValid: cpuValid, diskValid: true}
				if cpuValid {
					ticks := uint64(usage.UserTime + usage.SystemTime)
					reading.cpu = time.Duration(ticks * uint64(timebase.Numer) / uint64(timebase.Denom))
				}
				members[logProcessMember{pid: member, start: usage.Started}] = reading
			}
			return totals.reading(members), true
		},
	}
}
