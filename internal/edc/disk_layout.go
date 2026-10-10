package edc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// diskSystem은 disk 명령이 호스트와 닿는 통로다. sysfs와 /dev는 root 아래에서 읽으므로 테스트가
// 가짜 트리를 줄 수 있다. 사람용 lsblk 출력은 해석하지 않는다. route의 결함이 모두 텍스트 해석에서
// 나왔기 때문이다.
type diskSystem struct {
	root     string
	run      func(ctx context.Context, name string, args ...string) (string, error)
	lookPath func(name string) error
	xfsSize  func(mount string) (uint64, error)
	// capacity는 디스크 장치에 지금 크기를 묻는다. nil이면 묻지 않는다.
	capacity func(disk string) (uint64, error)
}

func (system diskSystem) path(parts ...string) string {
	return filepath.Join(append([]string{system.root}, parts...)...)
}

type diskMount struct {
	Point, FSType, Source, Root string
	Major, Minor                int
}

// parseMountInfo는 /proc/self/mountinfo를 읽는다. 공백은 \040으로 적혀 있다.
func parseMountInfo(data string) []diskMount {
	var mounts []diskMount
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		separator := -1
		for index, field := range fields {
			if field == "-" {
				separator = index
				break
			}
		}
		if separator < 6 || separator+2 >= len(fields) {
			continue
		}
		major, minor, ok := strings.Cut(fields[2], ":")
		if !ok {
			continue
		}
		majorNumber, err1 := strconv.Atoi(major)
		minorNumber, err2 := strconv.Atoi(minor)
		if err1 != nil || err2 != nil {
			continue
		}
		mounts = append(mounts, diskMount{
			Point: unescapeMountField(fields[4]), Root: unescapeMountField(fields[3]),
			FSType: fields[separator+1], Source: unescapeMountField(fields[separator+2]),
			Major: majorNumber, Minor: minorNumber,
		})
	}
	return mounts
}

func unescapeMountField(value string) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] == '\\' && index+3 < len(value) {
			if code, err := strconv.ParseUint(value[index+1:index+4], 8, 8); err == nil {
				builder.WriteByte(byte(code))
				index += 3
				continue
			}
		}
		builder.WriteByte(value[index])
	}
	return builder.String()
}

// findDiskMount는 path를 담는 가장 깊은 마운트를 고른다. 같은 지점에 여러 번 마운트했다면 마지막
// 마운트가 위를 덮으므로 뒤의 줄을 쓴다.
func findDiskMount(mounts []diskMount, path string) (diskMount, bool) {
	path = filepath.Clean(path)
	best, found := diskMount{}, false
	for _, mount := range mounts {
		if !pathUnder(path, mount.Point) {
			continue
		}
		if !found || len(mount.Point) >= len(best.Point) {
			best, found = mount, true
		}
	}
	return best, found
}

func pathUnder(path, mount string) bool {
	if mount == "/" || path == mount {
		return true
	}
	return strings.HasPrefix(path, mount+"/")
}

type diskBlock struct {
	Name      string
	Size      uint64
	Partition int
	Start     uint64
	Parent    string
	DMName    string
	DMUUID    string
	Slaves    []string
	Rescan    bool
	dir       string
}

func (block diskBlock) isDM() bool { return block.DMUUID != "" || block.DMName != "" }

func (block diskBlock) end() uint64 { return block.Start + block.Size }

func (system diskSystem) readUint(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}

func (system diskSystem) blockByNumber(major, minor int) (diskBlock, error) {
	resolved, err := filepath.EvalSymlinks(system.path("sys/dev/block", fmt.Sprintf("%d:%d", major, minor)))
	if err != nil {
		return diskBlock{}, err
	}
	return system.blockAt(resolved)
}

func (system diskSystem) blockByName(name string) (diskBlock, error) {
	resolved, err := filepath.EvalSymlinks(system.path("sys/class/block", name))
	if err != nil {
		return diskBlock{}, err
	}
	return system.blockAt(resolved)
}

// blockAt은 sysfs의 디바이스 디렉터리 하나를 읽는다. size와 start는 디바이스의 섹터 크기와 관계없이
// 늘 512 바이트 단위다.
func (system diskSystem) blockAt(dir string) (diskBlock, error) {
	block := diskBlock{Name: filepath.Base(dir), dir: dir}
	sectors, err := system.readUint(filepath.Join(dir, "size"))
	if err != nil {
		return diskBlock{}, err
	}
	block.Size = sectors * diskSectorSize
	if number, err := system.readUint(filepath.Join(dir, "partition")); err == nil {
		start, err := system.readUint(filepath.Join(dir, "start"))
		if err != nil {
			return diskBlock{}, err
		}
		block.Partition, block.Start = int(number), start*diskSectorSize
		block.Parent = filepath.Base(filepath.Dir(dir))
	}
	if data, err := os.ReadFile(filepath.Join(dir, "dm", "name")); err == nil {
		block.DMName = strings.TrimSpace(string(data))
	}
	if data, err := os.ReadFile(filepath.Join(dir, "dm", "uuid")); err == nil {
		block.DMUUID = strings.TrimSpace(string(data))
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "slaves")); err == nil {
		for _, entry := range entries {
			block.Slaves = append(block.Slaves, entry.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "device", "rescan")); err == nil && block.Partition == 0 {
		block.Rescan = true
	}
	return block, nil
}

// partitions는 디스크 아래의 파티션을 시작 위치 순서로 돌려준다. 번호 순서가 아니다. Ubuntu 클라우드
// 이미지는 루트가 1번이지만 디스크 맨 뒤에 있다.
func (system diskSystem) partitions(disk diskBlock) []diskBlock {
	entries, err := os.ReadDir(disk.dir)
	if err != nil {
		return nil
	}
	var parts []diskBlock
	for _, entry := range entries {
		if _, err := os.Stat(filepath.Join(disk.dir, entry.Name(), "partition")); err != nil {
			continue
		}
		if part, err := system.blockAt(filepath.Join(disk.dir, entry.Name())); err == nil {
			parts = append(parts, part)
		}
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Start < parts[j].Start })
	return parts
}

// partitionTable은 디스크 앞 1 KiB에서 GPT 헤더나 MBR 서명을 찾는다. 읽지 못하면 빈 문자열이다.
func (system diskSystem) partitionTable(disk string) string {
	file, err := os.Open(system.path("dev", disk))
	if err != nil {
		return ""
	}
	defer file.Close()
	head := make([]byte, 2*diskSectorSize)
	if _, err := file.ReadAt(head, 0); err != nil {
		return ""
	}
	if string(head[diskSectorSize:diskSectorSize+8]) == "EFI PART" {
		return "gpt"
	}
	if head[510] == 0x55 && head[511] == 0xAA {
		return "dos"
	}
	return ""
}

// ext4SuperblockSize는 ext 슈퍼블록에서 파일시스템 크기를 계산한다. statfs의 f_blocks는 메타데이터
// 몫을 빼므로 디바이스 크기와 비교할 수 없다.
func ext4SuperblockSize(superblock []byte) (uint64, error) {
	if len(superblock) < 0x158 || binary.LittleEndian.Uint16(superblock[0x38:]) != 0xEF53 {
		return 0, errors.New(T("disk.error.no_ext_superblock"))
	}
	blocks := uint64(binary.LittleEndian.Uint32(superblock[0x4:]))
	const incompat64Bit = 0x80
	if binary.LittleEndian.Uint32(superblock[0x60:])&incompat64Bit != 0 {
		blocks |= uint64(binary.LittleEndian.Uint32(superblock[0x150:])) << 32
	}
	return blocks * (1024 << binary.LittleEndian.Uint32(superblock[0x18:])), nil
}

func (system diskSystem) fsSize(mount diskMount, device string) (uint64, error) {
	if mount.FSType == "xfs" {
		return system.xfsSize(mount.Point)
	}
	file, err := os.Open(system.path("dev", device))
	if err != nil {
		return 0, err
	}
	defer file.Close()
	superblock := make([]byte, 1024)
	if _, err := file.ReadAt(superblock, 1024); err != nil {
		return 0, err
	}
	return ext4SuperblockSize(superblock)
}

type diskLV struct {
	VG, Name, Path string
	Size, VGFree   uint64
	ExtentSize     uint64
}

type diskPV struct {
	Path       string
	PEStart    uint64
	PECount    uint64
	ExtentSize uint64
}

// lvmReport는 `--reportformat json`의 모양이다. 값은 모두 문자열이다.
type lvmReport struct {
	Report []map[string][]map[string]string `json:"report"`
}

func (system diskSystem) lvmRows(ctx context.Context, kind string, command string, fields string) ([]map[string]string, error) {
	output, err := system.run(ctx, command, "--reportformat", "json", "--units", "b", "--nosuffix", "-o", fields)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", command, err)
	}
	var report lvmReport
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		return nil, fmt.Errorf("%s: %w", command, err)
	}
	var rows []map[string]string
	for _, part := range report.Report {
		rows = append(rows, part[kind]...)
	}
	return rows, nil
}

func lvmUint(row map[string]string, key string) uint64 {
	value, _ := strconv.ParseUint(strings.TrimSpace(row[key]), 10, 64)
	return value
}

func (system diskSystem) readLV(ctx context.Context, block diskBlock, major, minor int) (diskLV, error) {
	rows, err := system.lvmRows(ctx, "lv", "lvs", "vg_name,lv_name,lv_path,lv_size,vg_free,vg_extent_size,lv_kernel_major,lv_kernel_minor")
	if err != nil {
		return diskLV{}, err
	}
	for _, row := range rows {
		if row["lv_kernel_major"] == strconv.Itoa(major) && row["lv_kernel_minor"] == strconv.Itoa(minor) {
			return diskLV{
				VG: row["vg_name"], Name: row["lv_name"], Path: row["lv_path"],
				Size: lvmUint(row, "lv_size"), VGFree: lvmUint(row, "vg_free"), ExtentSize: lvmUint(row, "vg_extent_size"),
			}, nil
		}
	}
	return diskLV{}, errors.New(T("disk.error.lv_not_found", block.DMName))
}

func (system diskSystem) readPV(ctx context.Context, device string) (diskPV, error) {
	rows, err := system.lvmRows(ctx, "pv", "pvs", "pv_name,pe_start,pv_pe_count,vg_extent_size")
	if err != nil {
		return diskPV{}, err
	}
	for _, row := range rows {
		if row["pv_name"] == "/dev/"+device {
			return diskPV{
				Path: row["pv_name"], PEStart: lvmUint(row, "pe_start"),
				PECount: lvmUint(row, "pv_pe_count"), ExtentSize: lvmUint(row, "vg_extent_size"),
			}, nil
		}
	}
	return diskPV{}, errors.New(T("disk.error.pv_not_found", "/dev/"+device))
}

// diskChain은 파일시스템에서 디스크까지의 층이다. 비어 있는 층(LV, PV, 파티션)은 그 구성에 없다.
type diskChain struct {
	Mount     diskMount
	FS        diskBlock
	FSSize    uint64
	FSSizeErr error
	LV        *diskLV
	PV        *diskPV
	Part      *diskBlock
	Disk      diskBlock
	NextStart uint64
	LastEnd   uint64
	// LastPart는 디스크에서 가장 뒤에 끝나는 파티션이다. 마지막 파티션 뒤의 빈 공간은 이 파티션만 늘릴 수 있다.
	LastPart string
	DiskEnd  uint64
	Table    string
	Blocked  string
	// Volume은 장치가 알려 준 크기가 커널이 아는 크기보다 클 때 그 크기다. 클라우드 콘솔에서 늘린 SCSI 볼륨은
	// rescan 전까지 커널이 옛 크기를 본다. VolumeRead는 장치에 물어 답을 받았는지다.
	Volume     uint64
	VolumeRead bool
}

// afterRescan은 rescan한 뒤의 모습이다. 장치가 더 크다고 답했으면 디스크 크기를 그 값으로 바꾼다.
// check와 계획 출력이 쓰고, 실제 grow는 rescan한 뒤 커널 값을 다시 읽는다.
func (chain diskChain) afterRescan() diskChain {
	if chain.Volume == 0 {
		return chain
	}
	chain.Disk.Size = chain.Volume
	chain.DiskEnd = diskUsableEnd(chain.Volume, chain.Table, chain.Part != nil)
	return chain
}

// freeAfterLast는 이 파티션이 마지막이 아니고 마지막 파티션 뒤에 빈 공간이 있다는 뜻이다.
func (chain diskChain) freeAfterLast() bool {
	return chain.Part != nil && chain.NextStart > 0 && chain.LastPart != chain.Part.Name && chain.DiskEnd >= chain.LastEnd+diskGrowMinGap
}

// diskUsableEnd는 파티션이 닿을 수 있는 끝이다. 표를 읽지 못했다면(root가 아님) GPT로 보고 끝을 덜 잡는다.
// 늘릴 공간을 부풀리지 않는다.
func diskUsableEnd(size uint64, table string, partitioned bool) uint64 {
	if (table == "gpt" || (table == "" && partitioned)) && size > diskGPTTailBytes {
		return size - diskGPTTailBytes
	}
	return size
}

// fsDeviceSize는 파일시스템 바로 아래 층의 지금 크기다.
func (chain diskChain) fsDeviceSize() uint64 {
	switch {
	case chain.LV != nil:
		return chain.LV.Size
	case chain.Part != nil:
		return chain.Part.Size
	}
	return chain.Disk.Size
}

func (chain diskChain) fsDevicePath() string {
	if chain.LV != nil {
		return chain.LV.Path
	}
	return "/dev/" + chain.FS.Name
}

func (system diskSystem) mounts() ([]diskMount, error) {
	data, err := os.ReadFile(system.path("proc/self/mountinfo"))
	if err != nil {
		return nil, err
	}
	return parseMountInfo(string(data)), nil
}

// readChain은 마운트 하나의 층을 읽는다. 구조를 읽지 못하면 error를, 읽었지만 늘릴 수 없는 구성이면
// Blocked를 채운다. LVM 값은 root가 아니면 읽지 못할 수 있다.
func (system diskSystem) readChain(ctx context.Context, mount diskMount) (diskChain, error) {
	chain := diskChain{Mount: mount}
	if !diskGrowFSTypes[mount.FSType] {
		chain.Blocked = T("disk.blocked.fs_type", mount.FSType)
		return chain, nil
	}
	fsBlock, err := system.blockByNumber(mount.Major, mount.Minor)
	if err != nil {
		return chain, err
	}
	chain.FS = fsBlock
	below := fsBlock
	if fsBlock.isDM() {
		if !strings.HasPrefix(fsBlock.DMUUID, "LVM-") {
			chain.Blocked = T("disk.blocked.not_lvm", fsBlock.DMName)
			return chain, nil
		}
		if len(fsBlock.Slaves) != 1 {
			chain.Blocked = T("disk.blocked.many_pvs", fsBlock.DMName, len(fsBlock.Slaves))
			return chain, nil
		}
		lv, err := system.readLV(ctx, fsBlock, mount.Major, mount.Minor)
		if err != nil {
			return chain, err
		}
		chain.LV = &lv
		pv, err := system.readPV(ctx, fsBlock.Slaves[0])
		if err != nil {
			return chain, err
		}
		chain.PV = &pv
		if below, err = system.blockByName(fsBlock.Slaves[0]); err != nil {
			return chain, err
		}
	}
	if below.Partition > 0 {
		part := below
		chain.Part = &part
		if chain.Disk, err = system.blockByName(below.Parent); err != nil {
			return chain, err
		}
	} else {
		chain.Disk = below
	}
	chain.Table = system.partitionTable(chain.Disk.Name)
	chain.DiskEnd = diskUsableEnd(chain.Disk.Size, chain.Table, chain.Part != nil)
	if chain.Disk.Rescan && system.capacity != nil {
		if size, err := system.capacity(chain.Disk.Name); err == nil {
			chain.VolumeRead = true
			if size >= chain.Disk.Size+diskGrowMinGap {
				chain.Volume = size
			}
		}
	}
	if chain.Part != nil {
		if chain.Table == "dos" && chain.Part.Partition > 4 {
			chain.Blocked = T("disk.blocked.logical", chain.Part.Name)
		}
		for _, part := range system.partitions(chain.Disk) {
			if part.Start > chain.Part.Start && chain.NextStart == 0 {
				chain.NextStart = part.Start
			}
			if part.end() > chain.LastEnd {
				chain.LastEnd, chain.LastPart = part.end(), part.Name
			}
		}
	}
	chain.FSSize, chain.FSSizeErr = system.fsSize(mount, fsBlock.Name)
	return chain, nil
}
