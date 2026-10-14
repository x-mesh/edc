package edc

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseCPUKallsymsKeepsTextSymbols(t *testing.T) {
	symbols, err := parseCPUKallsyms(strings.NewReader(strings.Join([]string{
		"ffffffff81000300 T do_syscall_64",
		"ffffffff81000100 t helper",
		"ffffffff82000000 D jiffies",
		"0000000000000000 T hidden",
		"ffffffffc0001000 t nf_hook [nf_tables]",
		"ffffffff81000200 W weak_fn",
		"bad line",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(symbols) != 4 || symbols[0].name != "helper" || symbols[3].name != "nf_hook" {
		t.Fatalf("symbols = %+v", symbols)
	}
	for addr, want := range map[uint64]string{0xffffffff81000150: "helper", 0xffffffff81000300: "do_syscall_64", 0xffffffff810003ff: "do_syscall_64", 0xffffffffc0001010: "nf_hook"} {
		if got, ok := cpuLookup(symbols, addr); !ok || got != want {
			t.Errorf("lookup %x = %q %v, want %q", addr, got, ok, want)
		}
	}
	if _, ok := cpuLookup(symbols, 0xffffffff81000000); ok {
		t.Error("found a symbol below the first one")
	}
}

func TestCPULookupHonorsTheSymbolSize(t *testing.T) {
	symbols := cpuSortSymbols([]cpuSymbol{{addr: 0x200, size: 0x10, name: "b"}, {addr: 0x100, size: 0x20, name: "a"}})
	for addr, want := range map[uint64]string{0x100: "a", 0x11f: "a", 0x200: "b"} {
		if got, ok := cpuLookup(symbols, addr); !ok || got != want {
			t.Errorf("lookup %x = %q %v, want %q", addr, got, ok, want)
		}
	}
	for _, addr := range []uint64{0x120, 0x210, 0x50} {
		if got, ok := cpuLookup(symbols, addr); ok {
			t.Errorf("lookup %x = %q, want none", addr, got)
		}
	}
}

func TestParseCPUMapsKeepsExecutableMappings(t *testing.T) {
	mappings, err := parseCPUMaps(strings.NewReader(strings.Join([]string{
		"55d0c0000000-55d0c0001000 r--p 00000000 fd:01 1234 /usr/bin/app",
		"55d0c0001000-55d0c0005000 r-xp 00001000 fd:01 1234 /usr/bin/app",
		"7f0000000000-7f0000100000 r-xp 00028000 fd:01 99 /usr/lib/libc.so.6",
		"7f1000000000-7f1000100000 rwxp 00000000 00:00 0 ",
		"7ffd00000000-7ffd00002000 r-xp 00000000 00:00 0 [vdso]",
		"7f2000000000-7f2000001000 r-xp 00000000 fd:01 77 /tmp/old app (deleted)",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != 5 {
		t.Fatalf("mappings = %+v", mappings)
	}
	app, anon, vdso, deleted := mappings[0], mappings[2], mappings[3], mappings[4]
	if app.start != 0x55d0c0001000 || app.end != 0x55d0c0005000 || app.offset != 0x1000 || app.path != "/usr/bin/app" || app.key != "fd:01:1234" {
		t.Errorf("app = %+v", app)
	}
	if anon.path != "" || anon.key != "" || vdso.path != "[vdso]" || vdso.key != "" {
		t.Errorf("anon %+v vdso %+v", anon, vdso)
	}
	if deleted.path != "/tmp/old app (deleted)" || cpuFileBase(deleted.path) != "old app" {
		t.Errorf("deleted = %+v", deleted)
	}
}

func TestParseCPUPerfMapReadsJITSymbols(t *testing.T) {
	symbols := parseCPUPerfMap(strings.NewReader("7f00000a0000 40 py::hot:/app/x.py\n7f0000090000 10 [stub]\nnot a line\n7f00000b0000 0 empty\n"))
	if len(symbols) != 2 || symbols[0].name != "[stub]" {
		t.Fatalf("symbols = %+v", symbols)
	}
	if got, ok := cpuLookup(symbols, 0x7f00000a0010); !ok || got != "py::hot:/app/x.py" {
		t.Fatalf("lookup = %q %v", got, ok)
	}
	if _, ok := cpuLookup(symbols, 0x7f00000a0040); ok {
		t.Fatal("found a symbol past its size")
	}
}

func TestCPUSymbolizerNamesUnresolvedFrames(t *testing.T) {
	symbolizer := newCPUSymbolizer("/nonexistent")
	key := cpuProcessKey{TGID: 7, Comm: "app"}
	symbolizer.processes[key] = &cpuProcess{mappings: []cpuMapping{
		{start: 0x1000, end: 0x2000, path: "/usr/lib/libz.so.1", key: "fd:01:5"},
		{start: 0x3000, end: 0x4000, path: "[vdso]"},
		{start: 0x5000, end: 0x6000},
	}, perfMap: []cpuSymbol{{addr: 0x5100, size: 0x10, name: "jit_fn"}}}
	symbolizer.files["fd:01:5"] = &cpuFileRef{base: "libz.so.1", loaded: &cpuSymbolFile{base: "libz.so.1"}}
	for addr, want := range map[uint64]string{0x1500: "[libz.so.1]", 0x3500: "[vdso]", 0x5108: "jit_fn", 0x5200: "[anon]", 0x9000: "[unknown]"} {
		if got := symbolizer.userFrame(key, addr); got != want {
			t.Errorf("frame %x = %q, want %q", addr, got, want)
		}
	}
	if got := symbolizer.userFrame(cpuProcessKey{TGID: 8}, 0x1500); got != "[unknown]" {
		t.Errorf("frame of an unknown process = %q", got)
	}
	if got := symbolizer.kernelFrame(0xffffffff81000000); got != "[kernel]" {
		t.Errorf("kernel frame without kallsyms = %q", got)
	}
}

func TestCPUReportCountsSelfAndTotalOnce(t *testing.T) {
	stacks := []cpuStack{
		{TGID: 10, Process: "api", User: []string{"hash", "handle", "main"}, Samples: 6},
		{TGID: 10, Process: "api", User: []string{"read", "handle", "main"}, Kernel: []string{"copy_to_user", "sys_read"}, Samples: 3},
		// 재귀는 한 stack 안에서 한 번만 센다.
		{TGID: 11, Process: "worker", User: []string{"walk", "walk", "walk"}, Samples: 1},
	}
	report := cpuReportFor(stacks, cpuSummary{Samples: 100})
	summary := report.Summary
	if summary.Profiled != 10 || summary.Stacks != 3 || summary.ShallowUserSamples != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if len(report.Processes) != 2 || report.Processes[0].Process != "api" || report.Processes[0].Samples != 9 || report.Processes[0].Percent != 90 {
		t.Fatalf("processes = %+v", report.Processes)
	}
	rows := map[string]cpuFunctionRow{}
	for _, row := range report.Functions {
		key := row.Function
		if row.Kernel {
			key += cpuKernelSuffix
		}
		rows[key] = row
	}
	if report.Functions[0].Function != "hash" || rows["hash"].Self != 6 || rows["hash"].SelfPercent != 60 {
		t.Errorf("hash = %+v first %+v", rows["hash"], report.Functions[0])
	}
	if rows["copy_to_user_[k]"].Self != 3 || !rows["copy_to_user_[k]"].Kernel || rows["read"].Self != 0 || rows["read"].Total != 3 {
		t.Errorf("kernel self = %+v read %+v", rows["copy_to_user_[k]"], rows["read"])
	}
	if rows["handle"].Total != 9 || rows["main"].TotalPercent != 90 || rows["walk"].Total != 1 || rows["walk"].Self != 1 {
		t.Errorf("handle %+v main %+v walk %+v", rows["handle"], rows["main"], rows["walk"])
	}
	shallow := cpuReportFor([]cpuStack{{TGID: 1, User: []string{"leaf"}, Samples: 4}, {TGID: 1, User: []string{"a", "b"}, Samples: 1}}, cpuSummary{})
	if shallow.Summary.ShallowUserSamples != 4 {
		t.Errorf("shallow = %+v", shallow.Summary)
	}
}

func TestCPUFoldedLinesPutTheRootFirst(t *testing.T) {
	lines := cpuFoldedLines([]cpuStack{
		{Process: "api", User: []string{"hash", "main"}, Samples: 2},
		{Process: "api", User: []string{"read", "main"}, Kernel: []string{"copy_to_user", "ksys_read"}, Samples: 5},
		{Process: "api", User: []string{"hash", "main"}, Samples: 1},
		{Process: "odd name", User: []string{"a;b"}, Samples: 1},
		{Process: "", Samples: 4},
	})
	want := []string{
		"api;main;read;ksys_read_[k];copy_to_user_[k] 5",
		"-;[unknown] 4",
		"api;main;hash 3",
		"odd_name;a:b 1",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("folded =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestCPUShortenKeepsTheEnd(t *testing.T) {
	if got := cpuShorten("short", 10); got != "short" {
		t.Errorf("short = %q", got)
	}
	if got := cpuShorten("github.com/x/y.(*Server).handle", 12); got != "…ver).handle" || len([]rune(got)) != 12 {
		t.Errorf("long = %q", got)
	}
}
