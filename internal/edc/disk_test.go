package edc

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const gib = uint64(1) << 30

// fakeDiskHost는 sysfs, /dev, mountinfo를 임시 디렉터리에 만든다. 실제 커널처럼 class와 dev
// 아래의 이름은 devices 아래 디렉터리를 가리키는 symlink다.
type fakeDiskHost struct {
	t     *testing.T
	root  string
	calls []string
	// onRun은 명령이 실제로 바꿀 크기를 가짜 트리에 반영한다.
	onRun map[string]func(args []string)
	// lvm은 lvs와 pvs가 돌려줄 JSON이다.
	lvm      map[string]string
	missing  map[string]bool
	xfsSizes map[string]uint64
}

func newFakeDiskHost(t *testing.T) *fakeDiskHost {
	t.Helper()
	return &fakeDiskHost{
		t: t, root: t.TempDir(), onRun: map[string]func([]string){}, lvm: map[string]string{},
		missing: map[string]bool{}, xfsSizes: map[string]uint64{},
	}
}

func (host *fakeDiskHost) write(path string, data []byte) {
	host.t.Helper()
	full := filepath.Join(host.root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		host.t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		host.t.Fatal(err)
	}
}

func (host *fakeDiskHost) link(path, target string) {
	host.t.Helper()
	full := filepath.Join(host.root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		host.t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		host.t.Fatal(err)
	}
}

// block은 devices/<devPath>에 블록 디바이스를 만든다. start가 0보다 크거나 number가 있으면 파티션이다.
func (host *fakeDiskHost) block(devPath string, major, minor int, size uint64, number int, start uint64) {
	host.t.Helper()
	dir := filepath.Join("sys/devices", devPath)
	name := filepath.Base(devPath)
	host.setSize(name, devPath, size)
	if number > 0 {
		host.write(filepath.Join(dir, "partition"), []byte(strconv.Itoa(number)+"\n"))
		host.write(filepath.Join(dir, "start"), []byte(strconv.FormatUint(start/diskSectorSize, 10)+"\n"))
	}
	host.link(filepath.Join("sys/class/block", name), filepath.Join("../../devices", devPath))
	host.link(filepath.Join("sys/dev/block", fmt.Sprintf("%d:%d", major, minor)), filepath.Join("../../devices", devPath))
}

func (host *fakeDiskHost) setSize(name, devPath string, size uint64) {
	host.write(filepath.Join("sys/devices", devPath, "size"), []byte(strconv.FormatUint(size/diskSectorSize, 10)+"\n"))
}

func (host *fakeDiskHost) mountinfo(lines ...string) {
	host.write("proc/self/mountinfo", []byte(strings.Join(lines, "\n")+"\n"))
}

func (host *fakeDiskHost) gptDisk(name string) {
	head := make([]byte, 2*diskSectorSize)
	head[510], head[511] = 0x55, 0xAA
	copy(head[diskSectorSize:], "EFI PART")
	host.write(filepath.Join("dev", name), head)
}

func (host *fakeDiskHost) mbrDisk(name string) {
	head := make([]byte, 2*diskSectorSize)
	head[510], head[511] = 0x55, 0xAA
	host.write(filepath.Join("dev", name), head)
}

func (host *fakeDiskHost) ext4(name string, size uint64) {
	data := make([]byte, 2048)
	superblock := data[1024:]
	binary.LittleEndian.PutUint16(superblock[0x38:], 0xEF53)
	binary.LittleEndian.PutUint32(superblock[0x18:], 2) // 4096 바이트 블록
	binary.LittleEndian.PutUint32(superblock[0x4:], uint32(size/4096))
	host.write(filepath.Join("dev", name), data)
}

func (host *fakeDiskHost) system() diskSystem {
	return diskSystem{
		root: host.root,
		run: func(_ context.Context, name string, args ...string) (string, error) {
			host.calls = append(host.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
			if output, ok := host.lvm[name]; ok {
				return output, nil
			}
			if apply, ok := host.onRun[name]; ok {
				apply(args)
			}
			return "", nil
		},
		lookPath: func(name string) error {
			if host.missing[name] {
				return errors.New("not found")
			}
			return nil
		},
		xfsSize: func(mount string) (uint64, error) {
			size, ok := host.xfsSizes[mount]
			if !ok {
				return 0, errors.New("no xfs")
			}
			return size, nil
		},
	}
}

func (host *fakeDiskHost) chain(point string) diskChain {
	host.t.Helper()
	system := host.system()
	mounts, err := system.mounts()
	if err != nil {
		host.t.Fatal(err)
	}
	mount, ok := findDiskMount(mounts, point)
	if !ok {
		host.t.Fatalf("no mount for %s", point)
	}
	chain, err := system.readChain(context.Background(), mount)
	if err != nil {
		host.t.Fatal(err)
	}
	return chain
}

func stepCommands(steps []diskStep) []string {
	var commands []string
	for _, step := range steps {
		commands = append(commands, step.commandLine())
	}
	return commands
}

// ubuntuCloudDisk는 jw11에서 본 Ubuntu 24.04 클라우드 이미지 배치다. 루트가 1번이지만 맨 뒤에 있다.
func ubuntuCloudDisk(host *fakeDiskHost, diskSize, rootSize uint64) {
	host.block("pci0/virtio2/block/vda", 252, 0, diskSize, 0, 0)
	host.block("pci0/virtio2/block/vda/vda14", 252, 14, 4<<20, 14, 1<<20)
	host.block("pci0/virtio2/block/vda/vda15", 252, 15, 106<<20, 15, 5<<20)
	host.block("pci0/virtio2/block/vda/vda16", 252, 16, 913<<20, 16, 111<<20)
	host.block("pci0/virtio2/block/vda/vda1", 252, 1, rootSize, 1, 1025<<20)
	host.gptDisk("vda")
	host.ext4("vda1", rootSize)
	host.mountinfo(
		`29 1 252:1 / / rw,relatime shared:1 - ext4 /dev/vda1 rw`,
		`30 29 252:16 / /boot rw,relatime shared:2 - ext4 /dev/vda16 rw`,
		`31 29 0:26 / /run rw shared:3 - tmpfs tmpfs rw`,
	)
}

func TestParseMountInfoReadsEscapedPathsAndOptionalFields(t *testing.T) {
	mounts := parseMountInfo("36 35 98:0 /mnt1 /mnt\\040data rw,noatime master:1 shared:2 - xfs /dev/sdb1 rw\nbroken line\n")
	want := []diskMount{{Point: "/mnt data", Root: "/mnt1", FSType: "xfs", Source: "/dev/sdb1", Major: 98, Minor: 0}}
	if !reflect.DeepEqual(mounts, want) {
		t.Fatalf("mounts = %#v", mounts)
	}
}

func TestFindDiskMountPicksTheDeepestMount(t *testing.T) {
	mounts := []diskMount{{Point: "/"}, {Point: "/data"}, {Point: "/data2"}}
	for path, want := range map[string]string{"/data/x": "/data", "/data2": "/data2", "/datax": "/", "/": "/"} {
		mount, ok := findDiskMount(mounts, path)
		if !ok || mount.Point != want {
			t.Errorf("%s -> %q, want %q", path, mount.Point, want)
		}
	}
}

func TestExt4SuperblockSizeReadsThe64BitHigh(t *testing.T) {
	superblock := make([]byte, 1024)
	binary.LittleEndian.PutUint16(superblock[0x38:], 0xEF53)
	binary.LittleEndian.PutUint32(superblock[0x18:], 2)
	binary.LittleEndian.PutUint32(superblock[0x4:], 5)
	binary.LittleEndian.PutUint32(superblock[0x60:], 0x80)
	binary.LittleEndian.PutUint32(superblock[0x150:], 1)
	size, err := ext4SuperblockSize(superblock)
	if err != nil || size != ((1<<32)+5)*4096 {
		t.Fatalf("size = %d, %v", size, err)
	}
	if _, err := ext4SuperblockSize(make([]byte, 1024)); err == nil {
		t.Fatal("a superblock without the magic must fail")
	}
}

// 번호가 아니라 시작 위치로 마지막 파티션을 판정해야 Ubuntu 클라우드 이미지의 루트를 늘린다.
func TestPlanGrowsTheRootPartitionThatIsLastByStart(t *testing.T) {
	host := newFakeDiskHost(t)
	ubuntuCloudDisk(host, 30*gib, 19*gib)
	chain := host.chain("/")
	steps, blocked := planDiskGrow(chain)
	if blocked != "" {
		t.Fatalf("blocked: %s", blocked)
	}
	if got, want := stepCommands(steps), []string{"growpart /dev/vda 1", "resize2fs /dev/vda1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("steps = %q", got)
	}
	if steps[0].To != 30*gib-diskGPTTailBytes-1025<<20 {
		t.Fatalf("partition target = %d", steps[0].To)
	}
}

func TestPlanBlocksAPartitionThatIsNotLast(t *testing.T) {
	host := newFakeDiskHost(t)
	host.block("pci0/block/sdb", 8, 16, 30*gib, 0, 0)
	host.block("pci0/block/sdb/sdb1", 8, 17, 10*gib, 1, 1<<20)
	host.block("pci0/block/sdb/sdb2", 8, 18, 10*gib, 2, 1<<20+10*gib)
	host.gptDisk("sdb")
	host.ext4("sdb1", 10*gib)
	host.mountinfo(`40 1 8:17 / /data rw - ext4 /dev/sdb1 rw`)
	steps, blocked := planDiskGrow(host.chain("/data"))
	if len(steps) != 0 || !strings.Contains(blocked, "sdb1") {
		t.Fatalf("steps = %q, blocked = %q", stepCommands(steps), blocked)
	}
}

func TestPlanBlocksAnMBRDiskPastTwoTiB(t *testing.T) {
	host := newFakeDiskHost(t)
	host.block("pci0/block/sdc", 8, 32, 3<<40, 0, 0)
	host.block("pci0/block/sdc/sdc1", 8, 33, diskMBRLimitBytes-1<<20, 1, 1<<20)
	host.mbrDisk("sdc")
	host.ext4("sdc1", diskMBRLimitBytes-1<<20)
	host.mountinfo(`41 1 8:33 / /big rw - ext4 /dev/sdc1 rw`)
	steps, blocked := planDiskGrow(host.chain("/big"))
	if len(steps) != 0 || !strings.Contains(blocked, "sdc") {
		t.Fatalf("steps = %q, blocked = %q", stepCommands(steps), blocked)
	}
}

func lvmDisk(host *fakeDiskHost, diskSize, partSize uint64, peCount, vgFree, lvSize uint64) {
	host.block("pci0/virtio3/block/vdc", 252, 32, diskSize, 0, 0)
	host.block("pci0/virtio3/block/vdc/vdc1", 252, 33, partSize, 1, 1<<20)
	host.gptDisk("vdc")
	host.block("virtual/block/dm-0", 253, 0, lvSize, 0, 0)
	host.write("sys/devices/virtual/block/dm-0/dm/name", []byte("data--vg-data--lv\n"))
	host.write("sys/devices/virtual/block/dm-0/dm/uuid", []byte("LVM-abc\n"))
	host.write("sys/devices/virtual/block/dm-0/slaves/vdc1", nil)
	host.mountinfo(`50 1 253:0 / /srv rw - xfs /dev/mapper/data--vg-data--lv rw`)
	host.xfsSizes["/srv"] = lvSize
	host.lvm["lvs"] = fmt.Sprintf(`{"report":[{"lv":[{"vg_name":"data-vg","lv_name":"data-lv","lv_path":"/dev/data-vg/data-lv","lv_size":"%d","vg_free":"%d","vg_extent_size":"4194304","lv_kernel_major":"253","lv_kernel_minor":"0"}]}]}`, lvSize, vgFree)
	host.lvm["pvs"] = fmt.Sprintf(`{"report":[{"pv":[{"pv_name":"/dev/vdc1","pe_start":"1048576","pv_pe_count":"%d","vg_extent_size":"4194304"}]}]}`, peCount)
}

func TestPlanGrowsEveryLVMLayer(t *testing.T) {
	host := newFakeDiskHost(t)
	partSize := 10*gib - 1<<20
	extents := (partSize - 1<<20) / (4 << 20)
	lvmDisk(host, 20*gib, partSize, extents, 0, extents*(4<<20))
	steps, blocked := planDiskGrow(host.chain("/srv"))
	if blocked != "" {
		t.Fatalf("blocked: %s", blocked)
	}
	want := []string{"growpart /dev/vdc 1", "pvresize /dev/vdc1", "lvextend -l +100%FREE /dev/data-vg/data-lv", "xfs_growfs -d /srv"}
	if got := stepCommands(steps); !reflect.DeepEqual(got, want) {
		t.Fatalf("steps = %q", got)
	}
}

func TestPlanUsesFreeVGSpaceWithoutAPartitionStep(t *testing.T) {
	host := newFakeDiskHost(t)
	partSize := 20*gib - 1<<20 - diskGPTTailBytes
	extents := (partSize - 1<<20) / (4 << 20)
	lvmDisk(host, 20*gib, partSize, extents, 5*gib, extents*(4<<20)-5*gib)
	steps, _ := planDiskGrow(host.chain("/srv"))
	want := []string{"lvextend -l +100%FREE /dev/data-vg/data-lv", "xfs_growfs -d /srv"}
	if got := stepCommands(steps); !reflect.DeepEqual(got, want) {
		t.Fatalf("steps = %q", got)
	}
}

func TestCheckReportsAFullDiskAndLeavesOutOtherFileSystems(t *testing.T) {
	host := newFakeDiskHost(t)
	ubuntuCloudDisk(host, 20*gib, 20*gib-1025<<20-diskGPTTailBytes)
	host.block("pci0/block/sdd", 8, 48, 5*gib, 0, 0)
	host.write("sys/devices/pci0/block/sdd/device/rescan", nil)
	host.mountinfo(
		`29 1 252:1 / / rw - ext4 /dev/vda1 rw`,
		`30 29 8:48 / /snap rw - btrfs /dev/sdd rw`,
		`31 29 252:1 /var/lib/docker /mnt/bind rw - ext4 /dev/vda1 rw`,
	)
	results := checkDiskMounts(context.Background(), host.system(), "")
	if len(results) != 1 || results[0].Status != StatusPass {
		t.Fatalf("results = %#v, the bind mount and btrfs must be left out", results)
	}
	named := checkDiskMounts(context.Background(), host.system(), "/snap")
	if len(named) != 1 || named[0].Status != StatusSkip {
		t.Fatalf("a named btrfs mount = %#v", named)
	}
	if len(host.calls) != 0 {
		t.Fatalf("check ran %q", host.calls)
	}
}

func TestCheckWarnsWhenSpaceIsLeft(t *testing.T) {
	host := newFakeDiskHost(t)
	ubuntuCloudDisk(host, 30*gib, 19*gib)
	results := checkDiskMounts(context.Background(), host.system(), "/home/user")
	if len(results) != 1 || results[0].Status != StatusWarn || !strings.Contains(results[0].Summary, "edc disk grow /") {
		t.Fatalf("result = %#v", results)
	}
}

// growRootHost는 growpart와 resize2fs가 실제로 하는 일을 가짜 트리에 반영한다.
func growRootHost(t *testing.T) *fakeDiskHost {
	host := newFakeDiskHost(t)
	ubuntuCloudDisk(host, 30*gib, 19*gib)
	target := 30*gib - diskGPTTailBytes - 1025<<20
	host.onRun["growpart"] = func([]string) { host.setSize("vda1", "pci0/virtio2/block/vda/vda1", target) }
	host.onRun["resize2fs"] = func([]string) { host.ext4("vda1", target) }
	return host
}

func TestGrowRunsEachStepAndReportsTheNewSize(t *testing.T) {
	host := growRootHost(t)
	outcome := executeDiskGrow(context.Background(), host.system(), diskGrowInput{path: "/", confirm: diskAutoConfirm})
	if outcome.Result.Status != StatusPass {
		t.Fatalf("result = %#v", outcome.Result)
	}
	if want := []string{"growpart /dev/vda 1", "resize2fs /dev/vda1"}; !reflect.DeepEqual(host.calls, want) {
		t.Fatalf("calls = %q", host.calls)
	}
	if !strings.Contains(outcome.Result.Summary, "19.00 GB") || !strings.Contains(outcome.Result.Summary, "29.00 GB") {
		t.Fatalf("summary = %q", outcome.Result.Summary)
	}
}

// 중간에 끊긴 실행: 파티션은 이미 늘었고 파일시스템만 남았다.
func TestGrowResumesAfterThePartitionStep(t *testing.T) {
	host := growRootHost(t)
	host.onRun["growpart"](nil)
	outcome := executeDiskGrow(context.Background(), host.system(), diskGrowInput{path: "/", confirm: diskAutoConfirm})
	if outcome.Result.Status != StatusPass || !reflect.DeepEqual(host.calls, []string{"resize2fs /dev/vda1"}) {
		t.Fatalf("status = %s, calls = %q", outcome.Result.Status, host.calls)
	}
}

func TestGrowStopsWhenAStepDoesNotChangeTheSize(t *testing.T) {
	host := growRootHost(t)
	delete(host.onRun, "growpart")
	outcome := executeDiskGrow(context.Background(), host.system(), diskGrowInput{path: "/", confirm: diskAutoConfirm})
	if outcome.Result.Status != StatusFail || !reflect.DeepEqual(host.calls, []string{"growpart /dev/vda 1"}) {
		t.Fatalf("status = %s, calls = %q", outcome.Result.Status, host.calls)
	}
}

func TestGrowDryRunAndDeclineChangeNothing(t *testing.T) {
	host := growRootHost(t)
	host.write("sys/devices/pci0/virtio2/block/vda/device/rescan", nil)
	dryRun := executeDiskGrow(context.Background(), host.system(), diskGrowInput{path: "/", dryRun: true})
	if dryRun.Result.Status != StatusPass || len(host.calls) != 0 {
		t.Fatalf("dry run: status = %s, calls = %q", dryRun.Result.Status, host.calls)
	}
	declined := executeDiskGrow(context.Background(), host.system(), diskGrowInput{
		path: "/", confirm: func(string, string) (bool, error) { return false, nil },
	})
	// 거절해도 rescan은 이미 돌았다. rescan은 크기를 다시 읽게 할 뿐 데이터를 바꾸지 않는다.
	if !declined.Cancelled || !reflect.DeepEqual(host.calls, []string{"dd iflag=direct if=/dev/vda of=/dev/null count=1"}) {
		t.Fatalf("declined: cancelled = %v, calls = %q", declined.Cancelled, host.calls)
	}
	data, err := os.ReadFile(filepath.Join(host.root, "sys/class/block/vda/device/rescan"))
	if err != nil || string(data) != "1" {
		t.Fatalf("rescan file = %q, %v", data, err)
	}
}

func TestGrowRefusesWithoutGrowpart(t *testing.T) {
	host := growRootHost(t)
	host.missing["growpart"] = true
	outcome := executeDiskGrow(context.Background(), host.system(), diskGrowInput{path: "/", confirm: diskAutoConfirm})
	if outcome.Result.Status != StatusFail || len(host.calls) != 0 || !strings.Contains(outcome.Result.Summary, "cloud-guest-utils") {
		t.Fatalf("result = %#v, calls = %q", outcome.Result, host.calls)
	}
}

// 시간 제한이 지나도 디스크를 바꾸는 단계는 끊기지 않는다. 끊으면 커널은 확장을 끝내는데 edc만
// 실패로 보고한다.
func TestGrowStepsIgnoreAnExpiredTimeLimit(t *testing.T) {
	host := growRootHost(t)
	system := host.system()
	run := system.run
	var stepErrors []error
	system.run = func(ctx context.Context, name string, args ...string) (string, error) {
		stepErrors = append(stepErrors, ctx.Err())
		return run(ctx, name, args...)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome := executeDiskGrow(ctx, system, diskGrowInput{path: "/", confirm: diskAutoConfirm})
	if outcome.Result.Status != StatusPass || !reflect.DeepEqual(stepErrors, []error{nil, nil}) {
		t.Fatalf("status = %s, step context errors = %v", outcome.Result.Status, stepErrors)
	}
}

func TestGrowStopsAfterTheCurrentStepOnASignal(t *testing.T) {
	host := growRootHost(t)
	signals := make(chan os.Signal, 1)
	growpart := host.onRun["growpart"]
	host.onRun["growpart"] = func(args []string) {
		growpart(args)
		signals <- os.Interrupt
	}
	stopped := false
	outcome := executeDiskGrow(context.Background(), host.system(), diskGrowInput{
		path: "/", confirm: diskAutoConfirm,
		notify: func() (<-chan os.Signal, func()) { return signals, func() { stopped = true } },
	})
	if outcome.Result.Status != StatusFail || !reflect.DeepEqual(host.calls, []string{"growpart /dev/vda 1"}) || !stopped {
		t.Fatalf("status = %s, calls = %q, stopped = %v", outcome.Result.Status, host.calls, stopped)
	}
	if !strings.Contains(outcome.Result.Summary, "growpart /dev/vda 1") {
		t.Fatalf("summary = %q", outcome.Result.Summary)
	}
}

func TestDiskCommandMessageSkipsTheVersionBanner(t *testing.T) {
	for _, row := range []struct{ stderr, stdout, want string }{
		{"resize2fs 1.47.0 (5-Feb-2023)\nresize2fs: Permission denied to resize filesystem\n\n", "", "resize2fs: Permission denied to resize filesystem"},
		{"", "NOCHANGE: partition 1 is size 100. it cannot be grown\n", "NOCHANGE: partition 1 is size 100. it cannot be grown"},
		{"  \n", "", ""},
	} {
		if got := diskCommandMessage(row.stderr, row.stdout); got != row.want {
			t.Errorf("diskCommandMessage(%q, %q) = %q, want %q", row.stderr, row.stdout, got, row.want)
		}
	}
}

func TestGrowIgnoresASignalDuringTheLastStep(t *testing.T) {
	host := growRootHost(t)
	signals := make(chan os.Signal, 1)
	resize := host.onRun["resize2fs"]
	host.onRun["resize2fs"] = func(args []string) {
		resize(args)
		signals <- os.Interrupt
	}
	outcome := executeDiskGrow(context.Background(), host.system(), diskGrowInput{
		path: "/", confirm: diskAutoConfirm,
		notify: func() (<-chan os.Signal, func()) { return signals, func() {} },
	})
	if outcome.Result.Status != StatusPass {
		t.Fatalf("result = %#v", outcome.Result)
	}
}

func TestGrowCancelsOnASignalBeforeTheFirstStep(t *testing.T) {
	host := growRootHost(t)
	signals := make(chan os.Signal, 1)
	signals <- os.Interrupt
	outcome := executeDiskGrow(context.Background(), host.system(), diskGrowInput{
		path: "/", confirm: diskAutoConfirm,
		notify: func() (<-chan os.Signal, func()) { return signals, func() {} },
	})
	if !outcome.Cancelled || len(host.calls) != 0 {
		t.Fatalf("cancelled = %v, calls = %q", outcome.Cancelled, host.calls)
	}
}
