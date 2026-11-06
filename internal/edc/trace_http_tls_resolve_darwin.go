//go:build darwin

package edc

import (
	"context"
	"debug/macho"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

const darwinTLSTargetLimit = 16

func darwinTLSMappings(output string) map[uint32][]string {
	result := map[uint32][]string{}
	var pid uint32
	for _, line := range strings.Split(output, "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			value, err := strconv.ParseUint(line[1:], 10, 31)
			pid = 0
			if err == nil {
				pid = uint32(value)
			}
		case 'n':
			if pid != 0 && filepath.IsAbs(line[1:]) && !slices.Contains(result[pid], line[1:]) {
				result[pid] = append(result[pid], line[1:])
			}
		}
	}
	return result
}
func darwinTLSFileSymbols(path string) ([]string, error) {
	file, err := macho.Open(path)
	if err != nil {
		fat, fatErr := macho.OpenFat(path)
		if fatErr != nil {
			return nil, err
		}
		defer fat.Close()
		cpu := macho.CpuArm64
		if runtime.GOARCH == "amd64" {
			cpu = macho.CpuAmd64
		}
		for _, arch := range fat.Arches {
			if arch.Cpu == cpu {
				file = arch.File
				break
			}
		}
		if file == nil {
			return nil, fmt.Errorf("TLS Mach-O has no %s slice", runtime.GOARCH)
		}
	} else {
		defer file.Close()
	}
	cpu := macho.CpuArm64
	if runtime.GOARCH == "amd64" {
		cpu = macho.CpuAmd64
	}
	if file.Cpu != cpu {
		return nil, fmt.Errorf("TLS Mach-O architecture does not match %s", runtime.GOARCH)
	}
	if _, err := darwinTLSModuleName(path); err != nil {
		return nil, err
	}
	if file.Symtab == nil {
		return nil, nil
	}
	var symbols []string
	for _, symbol := range file.Symtab.Syms {
		if symbol.Type&0x0e == 0 || symbol.Value == 0 {
			continue
		}
		name := strings.TrimPrefix(symbol.Name, "_")
		if slices.Contains(darwinTLSFunctions, name) && !slices.Contains(symbols, name) {
			symbols = append(symbols, name)
		}
	}
	read := slices.Contains(symbols, "SSL_read") || slices.Contains(symbols, "SSL_read_ex")
	write := slices.Contains(symbols, "SSL_write") || slices.Contains(symbols, "SSL_write_ex")
	if !read || !write || !slices.Contains(symbols, "SSL_free") {
		return nil, nil
	}
	return symbols, nil
}

var readDarwinTLSMappings = func(ctx context.Context) (map[uint32][]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	stdout, stderr, err := runDarwinCheckTool(ctx, "/usr/sbin/lsof", "-nP", "-Fpn", "-d", "txt")
	if err != nil && stdout == "" {
		return nil, fmt.Errorf("find macOS TLS processes: %w: %s", err, strings.TrimSpace(stderr))
	}
	return darwinTLSMappings(stdout), nil
}

func findDarwinTLSTargets(ctx context.Context, mode traceTLSMode, process string) ([]traceTLSTarget, error) {
	wanted := ""
	var err error
	if mode != traceTLSAuto {
		wanted, err = filepath.EvalSymlinks(string(mode))
		if err != nil {
			return nil, fmt.Errorf("read TLS target: %w", err)
		}
		wanted, err = filepath.Abs(wanted)
		if err != nil {
			return nil, err
		}
		symbols, err := darwinTLSFileSymbols(wanted)
		if err != nil {
			return nil, err
		}
		if len(symbols) == 0 {
			return nil, fmt.Errorf("TLS target needs defined SSL read/write functions and SSL_free: %s", wanted)
		}
	}
	mappings, err := readDarwinTLSMappings(ctx)
	if err != nil {
		return nil, err
	}
	pids := make([]uint32, 0, len(mappings))
	for pid := range mappings {
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	type inspection struct{ symbols []string }
	inspected := map[string]inspection{}
	var targets []traceTLSTarget
	count := 0
	for _, pid := range pids {
		if pid == uint32(os.Getpid()) {
			continue
		}
		owner := darwinReadProcessOwner(int32(pid))
		if process != "" && owner.name != process {
			continue
		}
		found := false
		for i, path := range mappings[pid] {
			if wanted != "" {
				resolved, err := filepath.EvalSymlinks(path)
				if err != nil || resolved != wanted {
					continue
				}
				path = resolved
			} else if i != 0 && !(strings.HasPrefix(filepath.Base(path), "libssl") && strings.HasSuffix(path, ".dylib")) {
				continue
			}
			result, ok := inspected[path]
			if !ok {
				symbols, _ := darwinTLSFileSymbols(path)
				result = inspection{symbols}
				inspected[path] = result
			}
			if len(result.symbols) == 0 {
				continue
			}
			found = true
			targets = append(targets, traceTLSTarget{pid: pid, process: owner.name, path: path, symbols: result.symbols})
		}
		if found {
			count++
		}
	}
	if count > darwinTLSTargetLimit {
		return nil, fmt.Errorf("macOS TLS matches %d processes, limit %d; narrow --process or --tls=<path>", count, darwinTLSTargetLimit)
	}
	if wanted != "" && count == 0 {
		return nil, fmt.Errorf("no running process has TLS target loaded: %s", wanted)
	}
	return targets, nil
}
func resolveTraceHTTPTLSTargets(mode traceTLSMode, implicit bool, process string) (*traceTLSFinder, []string, int, error) {
	targets, err := findDarwinTLSTargets(context.Background(), mode, process)
	if err != nil {
		if implicit {
			return &traceTLSFinder{rescan: true}, []string{err.Error()}, 0, nil
		}
		return nil, nil, 3, err
	}
	finder := &traceTLSFinder{targets: targets, rescan: mode == traceTLSAuto}
	var notices []string
	if len(targets) == 0 {
		if !implicit {
			return nil, nil, 3, fmt.Errorf("no supported macOS OpenSSL process is loaded")
		}
		notices = append(notices, "No supported macOS OpenSSL process is loaded; TLS discovery will continue during capture")
	}
	return finder, notices, 0, nil
}
