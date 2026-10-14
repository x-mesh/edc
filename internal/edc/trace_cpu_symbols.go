package edc

import (
	"bufio"
	"debug/elf"
	"debug/gosym"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// trace cpu가 받은 주소에 함수 이름을 붙인다. kernel 주소는 /proc/kallsyms, 사용자 주소는 그 process의
// /proc/<pid>/maps가 가리키는 ELF의 심볼에서 찾는다. 심볼표를 지운 Go binary는 .gopclntab으로 찾고, JIT 코드는
// /tmp/perf-<pid>.map으로 찾는다.

type cpuSymbol struct {
	addr uint64
	size uint64
	name string
}

// cpuLookup은 addr를 덮는 심볼을 찾는다. 크기가 0인 심볼은 다음 심볼 전까지로 본다.
func cpuLookup(symbols []cpuSymbol, addr uint64) (string, bool) {
	index := sort.Search(len(symbols), func(i int) bool { return symbols[i].addr > addr }) - 1
	if index < 0 {
		return "", false
	}
	symbol := symbols[index]
	if symbol.size > 0 && addr >= symbol.addr+symbol.size {
		return "", false
	}
	return symbol.name, true
}

func cpuSortSymbols(symbols []cpuSymbol) []cpuSymbol {
	sort.Slice(symbols, func(i, j int) bool { return symbols[i].addr < symbols[j].addr })
	return symbols
}

// parseCPUKallsyms는 text 심볼만 남긴다. 주소가 모두 0이면 kptr_restrict가 주소를 가린 것이라 빈 표를 돌려준다.
func parseCPUKallsyms(reader io.Reader) ([]cpuSymbol, error) {
	var symbols []cpuSymbol
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		switch fields[1] {
		case "t", "T", "w", "W":
		default:
			continue
		}
		addr, err := strconv.ParseUint(fields[0], 16, 64)
		if err != nil || addr == 0 {
			continue
		}
		symbols = append(symbols, cpuSymbol{addr: addr, name: fields[2]})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return cpuSortSymbols(symbols), nil
}

type cpuMapping struct {
	start, end, offset uint64
	path               string
	// key는 같은 file을 한 번만 읽기 위한 dev와 inode다. 익명 mapping과 [vdso] 같은 특수 mapping은 비어 있다.
	key string
}

// parseCPUMaps는 실행 권한이 있는 mapping만 남긴다. 주소가 그 밖이면 함수가 아니다.
func parseCPUMaps(reader io.Reader) ([]cpuMapping, error) {
	var mappings []cpuMapping
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 || !strings.Contains(fields[1], "x") {
			continue
		}
		bounds := strings.SplitN(fields[0], "-", 2)
		if len(bounds) != 2 {
			continue
		}
		start, err1 := strconv.ParseUint(bounds[0], 16, 64)
		end, err2 := strconv.ParseUint(bounds[1], 16, 64)
		offset, err3 := strconv.ParseUint(fields[2], 16, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		mapping := cpuMapping{start: start, end: end, offset: offset}
		if len(fields) >= 6 {
			mapping.path = strings.Join(fields[5:], " ")
		}
		if fields[4] != "0" && strings.HasPrefix(mapping.path, "/") {
			mapping.key = fields[3] + ":" + fields[4]
		}
		mappings = append(mappings, mapping)
	}
	return mappings, scanner.Err()
}

// parseCPUPerfMap은 JIT가 쓰는 "START SIZE name" 줄을 읽는다. 두 수는 16진수다.
func parseCPUPerfMap(reader io.Reader) []cpuSymbol {
	var symbols []cpuSymbol
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.SplitN(strings.TrimSpace(scanner.Text()), " ", 3)
		if len(fields) != 3 {
			continue
		}
		addr, err1 := strconv.ParseUint(strings.TrimPrefix(fields[0], "0x"), 16, 64)
		size, err2 := strconv.ParseUint(strings.TrimPrefix(fields[1], "0x"), 16, 64)
		if err1 != nil || err2 != nil || size == 0 {
			continue
		}
		symbols = append(symbols, cpuSymbol{addr: addr, size: size, name: fields[2]})
	}
	return cpuSortSymbols(symbols)
}

// cpuSymbolFile은 ELF 하나의 심볼이다. loads는 file offset을 링크 주소로 바꾸는 데 쓴다.
type cpuSymbolFile struct {
	base    string
	loads   []elf.ProgHeader
	symbols []cpuSymbol
	gopcln  *gosym.Table
}

func loadCPUSymbolFile(reader io.ReaderAt, base string) (*cpuSymbolFile, error) {
	file, err := elf.NewFile(reader)
	if err != nil {
		return nil, err
	}
	result := &cpuSymbolFile{base: base}
	for _, program := range file.Progs {
		if program.Type == elf.PT_LOAD {
			result.loads = append(result.loads, program.ProgHeader)
		}
	}
	// .symtab은 .dynsym의 심볼을 모두 담는다. .symtab을 지운 library만 .dynsym을 읽는다.
	for _, read := range []func() ([]elf.Symbol, error){file.Symbols, file.DynamicSymbols} {
		symbols, err := read()
		if err != nil {
			continue
		}
		for _, symbol := range symbols {
			kind := elf.ST_TYPE(symbol.Info)
			if (kind != elf.STT_FUNC && kind != elf.STT_GNU_IFUNC) || symbol.Value == 0 || symbol.Section == elf.SHN_UNDEF {
				continue
			}
			// .dynsym의 이름에는 "pthread_mutex_lock@@GLIBC_2.2.5"처럼 symbol version이 붙는다. C 이름에는 '@'가 없다.
			name, _, _ := strings.Cut(symbol.Name, "@")
			result.symbols = append(result.symbols, cpuSymbol{addr: symbol.Value, size: symbol.Size, name: name})
		}
		if len(result.symbols) > 0 {
			break
		}
	}
	cpuSortSymbols(result.symbols)
	result.gopcln = cpuGoPCLN(file)
	return result, nil
}

// cpuPCLNMagics는 Go 버전별 pclntab header의 첫 4 byte다. 1.2, 1.16, 1.18, 1.20 순서다.
var cpuPCLNMagics = map[uint32]bool{0xfffffffb: true, 0xfffffffa: true, 0xfffffff0: true, 0xfffffff1: true}

// cpuGoPCLN은 Go binary의 pclntab을 읽는다. -s로 심볼표를 지워도 runtime이 쓰는 이 표는 남는다. Go binary가 아니면 nil이다.
// cgo binary는 .dynsym에 C 심볼 몇 개만 있어서, 심볼이 있어도 Go 함수는 이 표에서만 찾을 수 있다.
func cpuGoPCLN(file *elf.File) *gosym.Table {
	text := file.Section(".text")
	if text == nil {
		return nil
	}
	for _, candidate := range cpuPCLNCandidates(file) {
		table, err := gosym.NewTable(nil, gosym.NewLineTable(candidate.data, cpuGoText(file, text, candidate)))
		if err == nil && len(table.Funcs) > 0 {
			return table
		}
	}
	return nil
}

type cpuPCLN struct {
	data []byte
	addr uint64
}

// cpuPCLNCandidates는 pclntab일 수 있는 byte열과 그 주소다. 내부 linker는 .gopclntab section을 만든다. PIE를 외부
// linker로 build하면 그 section 없이 .data.rel.ro 안에 표가 들어가므로, header의 모양으로 찾는다.
func cpuPCLNCandidates(file *elf.File) []cpuPCLN {
	if section := file.Section(".gopclntab"); section != nil {
		if data, err := section.Data(); err == nil {
			return []cpuPCLN{{data, section.Addr}}
		}
	}
	var candidates []cpuPCLN
	for _, name := range []string{".data.rel.ro", ".rodata"} {
		section := file.Section(name)
		if section == nil {
			continue
		}
		data, err := section.Data()
		if err != nil {
			continue
		}
		for offset := 0; offset+8 <= len(data); offset += 4 {
			header := data[offset:]
			if !cpuPCLNMagics[file.ByteOrder.Uint32(header)] || header[4] != 0 || header[5] != 0 {
				continue
			}
			if quantum := header[6]; quantum != 1 && quantum != 2 && quantum != 4 {
				continue
			}
			if pointer := header[7]; pointer != 4 && pointer != 8 {
				continue
			}
			candidates = append(candidates, cpuPCLN{header, section.Addr + uint64(offset)})
		}
	}
	return candidates
}

// cpuModuleTextWord는 runtime.moduledata에서 text 칸의 위치다. 첫 칸이 pclntab header의 주소이고, 그 뒤에 slice 6개
// (word 18개), findfunctab, minpc, maxpc가 온 다음 text다. Go 1.16부터 이 순서다.
const cpuModuleTextWord = 22

// cpuGoText는 runtime.text의 주소다. 외부 linker로 build하면 C 시작 코드가 .text 앞에 오므로 runtime.text는 .text의
// 시작보다 뒤에 있고, .text의 시작을 넘기면 모든 함수 경계가 그만큼 어긋난다. runtime.firstmoduledata가 header의 주소와
// runtime.text를 함께 담으므로 그 값을 쓴다. 찾지 못하면 1.18 이상 header의 textStart, 그것도 0이면 .text의 시작이다.
func cpuGoText(file *elf.File, text *elf.Section, pcln cpuPCLN) uint64 {
	if len(pcln.data) < 8 {
		return text.Addr
	}
	pointer := int(pcln.data[7])
	word := func(data []byte) uint64 {
		if pointer == 4 {
			return uint64(file.ByteOrder.Uint32(data))
		}
		return file.ByteOrder.Uint64(data)
	}
	// Go 1.27부터 moduledata는 .go.module에 있고, 그 전에는 .noptrdata에 있다.
	for _, name := range []string{".go.module", ".noptrdata", ".data", ".data.rel.ro"} {
		section := file.Section(name)
		if section == nil || section.Type == elf.SHT_NOBITS {
			continue
		}
		data, err := section.Data()
		if err != nil {
			continue
		}
		last := len(data) - pointer*(cpuModuleTextWord+1)
		for offset := 0; offset <= last; offset += pointer {
			if word(data[offset:]) != pcln.addr {
				continue
			}
			if value := word(data[offset+pointer*cpuModuleTextWord:]); value >= text.Addr && value < text.Addr+text.Size {
				return value
			}
		}
	}
	if magic := file.ByteOrder.Uint32(pcln.data); (magic == 0xfffffff0 || magic == 0xfffffff1) && len(pcln.data) >= 8+3*pointer {
		if value := word(pcln.data[8+2*pointer:]); value != 0 {
			return value
		}
	}
	return text.Addr
}

// lookup은 file offset의 함수 이름이다. 실행 파일의 PT_LOAD로 offset을 링크 주소로 바꾼 뒤 찾는다.
func (file *cpuSymbolFile) lookup(fileOffset uint64) (string, bool) {
	addr, ok := fileOffset, false
	for _, load := range file.loads {
		if fileOffset >= load.Off && fileOffset < load.Off+load.Filesz {
			addr, ok = fileOffset-load.Off+load.Vaddr, true
			break
		}
	}
	if !ok {
		return "", false
	}
	// Go 함수를 먼저 찾는다. 크기가 0인 C 심볼은 다음 심볼까지 덮으므로, 먼저 찾으면 그 뒤의 Go 함수를 그 이름으로 잘못 붙인다.
	if file.gopcln != nil {
		if function := file.gopcln.PCToFunc(addr); function != nil {
			return function.Name, true
		}
	}
	return cpuLookup(file.symbols, addr)
}

// cpuProcessKey는 snapshot 하나다. exec는 같은 tgid의 mapping을 바꾸고 comm도 바꾸므로, exec 전후의 샘플이 각자의
// mapping으로 이름을 찾는다.
type cpuProcessKey struct {
	TGID uint32
	Comm string
}

// cpuFileRef는 mapping된 file 하나다. snapshot은 file을 열어 두기만 하고, 심볼표는 끝날 때 stack에 나온 file만 읽는다.
// 바쁜 host에는 mapping된 file이 수백 개이고 그 대부분은 샘플에 나오지 않는다. 열어 둔 fd는 process가 끝난 뒤에도 읽힌다.
type cpuFileRef struct {
	base   string
	handle *os.File
	loaded *cpuSymbolFile
	err    error
}

type cpuProcess struct {
	mappings []cpuMapping
	perfMap  []cpuSymbol
	err      error
}

// cpuSymbolizer는 process와 file의 심볼을 모은다. process가 끝나면 /proc에서 사라지므로, trace 중에 처음 본 process를
// snapshot으로 바로 읽어 둔다.
type cpuSymbolizer struct {
	proc      string
	kernel    []cpuSymbol
	processes map[cpuProcessKey]*cpuProcess
	files     map[string]*cpuFileRef
}

func newCPUSymbolizer(proc string) *cpuSymbolizer {
	return &cpuSymbolizer{proc: proc, processes: map[cpuProcessKey]*cpuProcess{}, files: map[string]*cpuFileRef{}}
}

func (symbolizer *cpuSymbolizer) loadKernel() error {
	file, err := os.Open(filepath.Join(symbolizer.proc, "kallsyms"))
	if err != nil {
		return err
	}
	defer file.Close()
	symbolizer.kernel, err = parseCPUKallsyms(file)
	return err
}

// snapshot은 process의 mapping, 그 file의 심볼, perf map을 읽는다. 이미 읽은 process는 새 mapping만 더한다. exec 도중에
// 읽으면 comm은 바뀌었어도 공유 library가 아직 mapping되지 않았으므로, 다시 읽어 채운다. 이미 끝난 process는 처음 읽은
// 것을 그대로 둔다.
func (symbolizer *cpuSymbolizer) snapshot(key cpuProcessKey) {
	process := symbolizer.processes[key]
	first := process == nil
	if first {
		process = &cpuProcess{}
		symbolizer.processes[key] = process
	}
	dir := filepath.Join(symbolizer.proc, strconv.FormatUint(uint64(key.TGID), 10))
	maps, err := os.Open(filepath.Join(dir, "maps"))
	if err != nil {
		if len(process.mappings) == 0 {
			process.err = err
		}
		return
	}
	mappings, err := parseCPUMaps(maps)
	maps.Close()
	if err != nil {
		return
	}
	process.err = nil
	known := map[cpuMapping]bool{}
	for _, mapping := range process.mappings {
		known[mapping] = true
	}
	for _, mapping := range mappings {
		if !known[mapping] {
			process.mappings = append(process.mappings, mapping)
		}
	}
	for _, mapping := range mappings {
		if mapping.key == "" || symbolizer.files[mapping.key] != nil {
			continue
		}
		// map_files는 지워진 file과 다른 mount namespace의 file도 연다. 열지 못하면 그 process의 root에서 경로로 연다.
		candidates := []string{
			filepath.Join(dir, "map_files", fmt.Sprintf("%x-%x", mapping.start, mapping.end)),
			filepath.Join(dir, "root", mapping.path),
		}
		ref := &cpuFileRef{base: cpuFileBase(mapping.path)}
		for _, candidate := range candidates {
			if ref.handle, ref.err = os.Open(candidate); ref.err == nil {
				break
			}
		}
		symbolizer.files[mapping.key] = ref
	}
	// perf map은 처음과 끝에만 읽는다. 끝의 refreshPerfMaps가 trace 중에 JIT가 더한 함수를 채운다.
	if first {
		process.perfMap = symbolizer.readPerfMap(key.TGID)
	}
}

// refreshPerfMaps는 끝날 때 아직 살아 있는 process의 perf map을 다시 읽는다. JIT는 trace 중에도 새 함수를 쓴다.
func (symbolizer *cpuSymbolizer) refreshPerfMaps() {
	for key, process := range symbolizer.processes {
		if perfMap := symbolizer.readPerfMap(key.TGID); perfMap != nil {
			process.perfMap = perfMap
		}
	}
}

// readPerfMap은 process의 mount namespace 안 /tmp에 있는 perf map을 읽는다. 파일 이름은 그 namespace의 pid다.
func (symbolizer *cpuSymbolizer) readPerfMap(tgid uint32) []cpuSymbol {
	dir := filepath.Join(symbolizer.proc, strconv.FormatUint(uint64(tgid), 10))
	nspid := cpuNamespacePID(filepath.Join(dir, "status"), tgid)
	file, err := os.Open(filepath.Join(dir, "root", "tmp", fmt.Sprintf("perf-%d.map", nspid)))
	if err != nil {
		return nil
	}
	defer file.Close()
	return parseCPUPerfMap(file)
}

// cpuNamespacePID는 status의 NSpid 줄에서 가장 안쪽 namespace의 pid를 읽는다. 줄이 없으면 tgid 그대로다.
func cpuNamespacePID(status string, tgid uint32) uint32 {
	data, err := os.ReadFile(status)
	if err != nil {
		return tgid
	}
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "NSpid:" {
			if pid, err := strconv.ParseUint(fields[len(fields)-1], 10, 32); err == nil {
				return uint32(pid)
			}
		}
	}
	return tgid
}

// symbolFile은 key의 심볼표를 처음 쓸 때 읽고 fd를 닫는다. 읽지 못한 file은 nil이다.
func (symbolizer *cpuSymbolizer) symbolFile(key string) (*cpuFileRef, *cpuSymbolFile) {
	ref := symbolizer.files[key]
	if ref == nil {
		return nil, nil
	}
	if ref.handle != nil {
		ref.loaded, ref.err = loadCPUSymbolFile(ref.handle, ref.base)
		ref.handle.Close()
		ref.handle = nil
	}
	return ref, ref.loaded
}

// close는 끝까지 쓰지 않은 file의 fd를 닫는다.
func (symbolizer *cpuSymbolizer) close() {
	for _, ref := range symbolizer.files {
		if ref.handle != nil {
			ref.handle.Close()
			ref.handle = nil
		}
	}
}

// kernelFrame은 kernel 주소의 이름이다. kallsyms를 읽지 못했으면 [kernel]이다.
func (symbolizer *cpuSymbolizer) kernelFrame(addr uint64) string {
	if name, ok := cpuLookup(symbolizer.kernel, addr); ok {
		return name
	}
	return "[kernel]"
}

// userFrame은 사용자 주소의 이름이다. 함수를 찾지 못하면 mapping의 file 이름을, mapping도 없으면 [unknown]을 쓴다.
// flame graph에서 같은 file의 주소가 한 칸으로 모이도록 offset은 붙이지 않는다.
func (symbolizer *cpuSymbolizer) userFrame(key cpuProcessKey, addr uint64) string {
	process := symbolizer.processes[key]
	if process == nil {
		return "[unknown]"
	}
	if name, ok := cpuLookup(process.perfMap, addr); ok {
		return name
	}
	for _, mapping := range process.mappings {
		if addr < mapping.start || addr >= mapping.end {
			continue
		}
		if ref, file := symbolizer.symbolFile(mapping.key); ref != nil {
			if file != nil {
				if name, ok := file.lookup(addr - mapping.start + mapping.offset); ok {
					return name
				}
			}
			return "[" + ref.base + "]"
		}
		switch {
		case strings.HasPrefix(mapping.path, "["):
			return mapping.path
		case mapping.path != "":
			return "[" + cpuFileBase(mapping.path) + "]"
		}
		return "[anon]"
	}
	return "[unknown]"
}

// cpuFileBase는 maps가 지워진 file 뒤에 붙이는 " (deleted)"를 뗀 file 이름이다.
func cpuFileBase(path string) string {
	return filepath.Base(strings.TrimSuffix(path, " (deleted)"))
}
