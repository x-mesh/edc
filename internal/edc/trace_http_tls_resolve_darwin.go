//go:build darwin

package edc

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"debug/elf"
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
func openDarwinTLSFile(path string) (*macho.File, func() error, error) {
	file, err := macho.Open(path)
	if err != nil {
		fat, fatErr := macho.OpenFat(path)
		if fatErr != nil {
			return nil, nil, err
		}
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
			_ = fat.Close()
			return nil, nil, fmt.Errorf("TLS Mach-O has no %s slice", runtime.GOARCH)
		}
		return file, fat.Close, nil
	}
	cpu := macho.CpuArm64
	if runtime.GOARCH == "amd64" {
		cpu = macho.CpuAmd64
	}
	if file.Cpu != cpu {
		_ = file.Close()
		return nil, nil, fmt.Errorf("TLS Mach-O architecture does not match %s", runtime.GOARCH)
	}
	return file, file.Close, nil
}
func darwinTLSDefinedSymbols(file *macho.File, names []string) []string {
	var symbols []string
	if file.Symtab == nil {
		return nil
	}
	for _, symbol := range file.Symtab.Syms {
		if symbol.Type&0x0e == 0 || symbol.Value == 0 {
			continue
		}
		name := strings.TrimPrefix(symbol.Name, "_")
		if slices.Contains(names, name) && !slices.Contains(symbols, name) {
			symbols = append(symbols, name)
		}
	}
	return symbols
}
func darwinTLSFileSymbols(path string) ([]string, error) {
	file, closeFile, err := openDarwinTLSFile(path)
	if err != nil {
		return nil, err
	}
	defer closeFile()
	if _, err := darwinTLSModuleName(path); err != nil {
		return nil, err
	}
	symbols := darwinTLSDefinedSymbols(file, darwinTLSFunctions)
	if !(slices.Contains(symbols, "SSL_read") || slices.Contains(symbols, "SSL_read_ex")) || !(slices.Contains(symbols, "SSL_write") || slices.Contains(symbols, "SSL_write_ex")) || !slices.Contains(symbols, "SSL_free") {
		return nil, nil
	}
	return symbols, nil
}
func inspectDarwinTLSFile(path string) ([]traceTLSTarget, error) {
	file, closeFile, err := openDarwinTLSFile(path)
	if err != nil {
		return nil, err
	}
	defer closeFile()
	var targets []traceTLSTarget
	symbols := darwinTLSDefinedSymbols(file, darwinTLSFunctions)
	if (slices.Contains(symbols, "SSL_read") || slices.Contains(symbols, "SSL_read_ex")) && (slices.Contains(symbols, "SSL_write") || slices.Contains(symbols, "SSL_write_ex")) && slices.Contains(symbols, "SSL_free") {
		if _, err := darwinTLSModuleName(path); err != nil {
			return nil, err
		}
		targets = append(targets, traceTLSTarget{path: path, symbols: symbols})
	}
	goSymbols := darwinTLSDefinedSymbols(file, []string{traceTLSGoRead, traceTLSGoWrite, traceTLSGoClose})
	hasGoTLS := len(goSymbols) > 0
	if section := file.Section("__gopclntab"); section != nil && !hasGoTLS {
		if section.Size > traceTLSGoMaxTable {
			return nil, fmt.Errorf("Go TLS function table exceeds capture limit")
		}
		data, err := section.Data()
		if err != nil {
			return nil, err
		}
		hasGoTLS = bytes.Contains(data, []byte(traceTLSGoRead)) && bytes.Contains(data, []byte(traceTLSGoWrite))
	}
	if hasGoTLS {
		info, err := buildinfo.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read Go TLS ABI: %w", err)
		}
		if info.GoVersion != "go1.25.0" && info.GoVersion != "go1.27.3" {
			return nil, fmt.Errorf("unsupported macOS Go TLS ABI: %s", info.GoVersion)
		}
		names := []string{traceTLSGoRead, traceTLSGoWrite, traceTLSGoClose, "internal/poll.(*FD).Read", "internal/poll.(*FD).Write"}
		symbols = darwinTLSDefinedSymbols(file, names)
		if len(symbols) != len(names) {
			return nil, fmt.Errorf("macOS Go TLS needs defined TLS and poll FD symbols; stripped binaries are unsupported")
		}
		if _, err := darwinTLSModuleName(path); err != nil {
			return nil, err
		}
		machine := elf.EM_AARCH64
		if file.Cpu == macho.CpuAmd64 {
			machine = elf.EM_X86_64
		}
		targets = append(targets, traceTLSTarget{path: path, symbols: []string{traceTLSGoRead, traceTLSGoWrite, traceTLSGoClose}, goMachine: machine})
	}
	imports, err := file.ImportedSymbols()
	if err != nil {
		return nil, err
	}
	for i := range imports {
		imports[i] = strings.TrimPrefix(imports[i], "_")
	}
	if slices.Contains(imports, "SSLRead") && slices.Contains(imports, "SSLWrite") {
		libraries, err := file.ImportedLibraries()
		if err != nil {
			return nil, err
		}
		for _, library := range libraries {
			if strings.HasPrefix(library, "/System/Library/Frameworks/Security.framework/") {
				targets = append(targets, traceTLSTarget{path: path, darwinModule: "Security", symbols: []string{"SSLRead", "SSLWrite", "SSLClose", "SSLDisposeContext"}})
				break
			}
		}
	}
	return targets, nil
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
		targets, err := inspectDarwinTLSFile(wanted)
		if err != nil {
			return nil, err
		}
		if len(targets) == 0 {
			return nil, fmt.Errorf("TLS target has no supported macOS TLS implementation: %s", wanted)
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
	type inspection struct {
		targets []traceTLSTarget
		err     error
	}
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
				fileTargets, inspectErr := inspectDarwinTLSFile(path)
				result = inspection{fileTargets, inspectErr}
				inspected[path] = result
			}
			if result.err != nil && wanted != "" {
				return nil, result.err
			}
			if len(result.targets) == 0 {
				continue
			}
			found = true
			for _, target := range result.targets {
				target.pid = pid
				target.process = owner.name
				targets = append(targets, target)
			}
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
			return nil, nil, 3, fmt.Errorf("no supported macOS TLS process is loaded")
		}
		notices = append(notices, "No supported macOS TLS process is loaded; TLS discovery will continue during capture")
	}
	return finder, notices, 0, nil
}
