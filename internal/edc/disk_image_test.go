package edc

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

func loadDiskImageFixture(t *testing.T) []diskImage {
	t.Helper()
	data, err := os.ReadFile("testdata/hdiutil-info.plist")
	if err != nil {
		t.Fatal(err)
	}
	images, err := parseDiskImages(data)
	if err != nil {
		t.Fatal(err)
	}
	return images
}

func TestParseDiskImagesReadsDevicesMountsAndSize(t *testing.T) {
	images := loadDiskImageFixture(t)
	if len(images) != 5 {
		t.Fatalf("images = %d, want 5", len(images))
	}
	kiro := images[1]
	if kiro.Path != "/Users/me/Downloads/Kiro CLI.dmg" || kiro.Device != "/dev/disk8" || kiro.Writable {
		t.Fatalf("kiro = %+v", kiro)
	}
	if !slices.Equal(kiro.Mounts, []string{"/Volumes/Kiro CLI"}) || !slices.Equal(kiro.Devices, []string{"/dev/disk8", "/dev/disk8s1"}) {
		t.Fatalf("kiro entities = %+v", kiro)
	}
	if kiro.Size != 1171912*512 {
		t.Fatalf("kiro size = %d", kiro.Size)
	}
}

func TestLeftoverDiskImagesSkipSystemWritableAndHiddenImages(t *testing.T) {
	var mounts []string
	for _, image := range leftoverDiskImages(loadDiskImageFixture(t)) {
		mounts = append(mounts, image.volumeMount())
	}
	// cryptex, simulator runtime, 쓸 수 있는 sparse image는 사용자가 쓰는 중일 수 있어 남긴다.
	if want := []string{"/Volumes/Kiro CLI", "/Volumes/Kiro CLI 1"}; !slices.Equal(mounts, want) {
		t.Fatalf("leftover = %v, want %v", mounts, want)
	}
}

func TestMarkDiskImagesHidesOnlyLeftoverImagesInInfo(t *testing.T) {
	disks := []diskDetails{
		{Mount: "/", Device: "/dev/disk3s1s1", Total: 100, Used: 50, Percent: 50},
		{Mount: "/Volumes/Kiro CLI", Device: "/dev/disk8s1", Total: 10, Used: 10, Percent: 100},
		{Mount: "/private/tmp/Kiro CLI 1", Device: "/dev/disk9s1", Total: 10, Used: 10, Percent: 100},
		{Mount: "/Volumes/scratch", Device: "/dev/disk11s1", Total: 10, Used: 1, Percent: 10},
		{Mount: "/Library/Developer/CoreSimulator/Volumes/iOS_23A343", Device: "/dev/disk12s1", Total: 10, Used: 10, Percent: 100},
	}
	markDiskImages(disks, loadDiskImageFixture(t))
	var hidden []string
	for _, disk := range disks {
		if hiddenDisk(disk) {
			hidden = append(hidden, disk.Mount)
		}
	}
	// df가 경로를 다르게 적어도 장치 이름으로 같은 이미지를 찾는다.
	if want := []string{"/Volumes/Kiro CLI", "/private/tmp/Kiro CLI 1"}; !slices.Equal(hidden, want) {
		t.Fatalf("hidden = %v, want %v", hidden, want)
	}
	var output bytes.Buffer
	printDiskUsage(&output, disks, nil, false, false)
	if !strings.Contains(output.String(), "2 disk images hidden · edc disk check") || strings.Contains(output.String(), "Kiro") {
		t.Fatalf("output:\n%s", output.String())
	}
}

func TestPrintDiskUsageHasNoImageLineWithoutImages(t *testing.T) {
	var output bytes.Buffer
	printDiskUsage(&output, []diskDetails{{Mount: "/", Device: "/dev/disk3s1s1", Total: 100, Used: 50, Percent: 50}}, nil, false, false)
	if strings.Contains(output.String(), "disk images hidden") {
		t.Fatalf("output:\n%s", output.String())
	}
}

func TestCheckDiskImagesListsLeftoverImagesWithTheirFiles(t *testing.T) {
	images := loadDiskImageFixture(t)
	result := checkDiskImages(images, "")
	if result.Status != StatusPass || result.Next != "edc disk detach" || len(result.Evidence) != 2 {
		t.Fatalf("result = %+v", result)
	}
	if result.Evidence[0].Label != "/Volumes/Kiro CLI" || !strings.HasPrefix(result.Evidence[0].Value, "/Users/me/Downloads/Kiro CLI.dmg · ") {
		t.Fatalf("evidence = %+v", result.Evidence[0])
	}
	if one := checkDiskImages(images, "/Volumes/Kiro CLI 1/"); len(one.Evidence) != 1 || one.Evidence[0].Label != "/Volumes/Kiro CLI 1" {
		t.Fatalf("one = %+v", one)
	}
	if none := checkDiskImages(images[:1], ""); none.Next != "" || none.Summary != T("disk.image.check.none") {
		t.Fatalf("none = %+v", none)
	}
	if wrong := checkDiskImages(images, "/Volumes/scratch"); wrong.Status != StatusSkip {
		t.Fatalf("writable image accepted: %+v", wrong)
	}
}

type fakeDetach struct {
	calls []string
	fail  map[string]string
}

func (fake *fakeDetach) run(_ context.Context, args ...string) (string, string, error) {
	fake.calls = append(fake.calls, strings.Join(args, " "))
	if message, ok := fake.fail[args[len(args)-1]]; ok {
		return "", message + "\n", errors.New("exit status 16")
	}
	return "", "", nil
}

func TestExecuteDiskDetachDryRunChangesNothing(t *testing.T) {
	fake := &fakeDetach{}
	outcome := executeDiskDetach(context.Background(), loadDiskImageFixture(t), diskDetachInput{dryRun: true, run: fake.run})
	if len(fake.calls) != 0 || outcome.Result.Status != StatusPass || len(outcome.Result.Evidence) != 2 {
		t.Fatalf("calls = %v, outcome = %+v", fake.calls, outcome)
	}
	if !strings.HasPrefix(outcome.Result.Evidence[0].Value, "hdiutil detach /dev/disk8 · ") {
		t.Fatalf("plan = %+v", outcome.Result.Evidence)
	}
}

func TestExecuteDiskDetachStopsWhenNotConfirmed(t *testing.T) {
	fake := &fakeDetach{}
	refuse := func(string, string) (bool, error) { return false, nil }
	outcome := executeDiskDetach(context.Background(), loadDiskImageFixture(t), diskDetachInput{confirm: refuse, run: fake.run})
	if !outcome.Cancelled || len(fake.calls) != 0 {
		t.Fatalf("calls = %v, outcome = %+v", fake.calls, outcome)
	}
}

func TestExecuteDiskDetachKeepsGoingAfterABusyImage(t *testing.T) {
	fake := &fakeDetach{fail: map[string]string{"/dev/disk8": `hdiutil: couldn't unmount "disk8" - Resource busy`}}
	outcome := executeDiskDetach(context.Background(), loadDiskImageFixture(t), diskDetachInput{confirm: diskAutoConfirm, run: fake.run})
	if want := []string{"detach /dev/disk8", "detach /dev/disk9"}; !slices.Equal(fake.calls, want) {
		t.Fatalf("calls = %v, want %v", fake.calls, want)
	}
	result := outcome.Result
	if result.Status != StatusFail || result.Summary != T("disk.image.detach.summary", 1, 2) || len(result.Warnings) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Evidence[0].Value, "Resource busy") || !strings.Contains(result.Evidence[1].Value, T("disk.image.detach.done")) {
		t.Fatalf("evidence = %+v", result.Evidence)
	}
}

func TestExecuteDiskDetachRefusesAMountThatIsNotALeftoverImage(t *testing.T) {
	fake := &fakeDetach{}
	outcome := executeDiskDetach(context.Background(), loadDiskImageFixture(t), diskDetachInput{mounts: []string{"/"}, confirm: diskAutoConfirm, run: fake.run})
	if outcome.Result.Status != StatusFail || len(fake.calls) != 0 {
		t.Fatalf("calls = %v, outcome = %+v", fake.calls, outcome)
	}
}
