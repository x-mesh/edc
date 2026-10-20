package edc

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 체인 ID는 docker의 규칙(chain1 = diff1, chainN = sha256(chain(N-1) + " " + diffN))으로 따로 계산해 둔 값이다.
const (
	layerDiff1  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	layerDiff2  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	layerDiff3  = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	layerChain1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	layerChain2 = "sha256:9932074217c35353d2e03a3f5a86549f7c67bfeb0ba53e23d69f4d4c8f7958f5"
	layerChain3 = "sha256:50127c0d6b338be78246434b4bce23ecf75e505158ea0bbd15effeb162fbf3ec"

	layerImageX = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	layerImageY = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	layerImageZ = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	layerBoxID  = "c0ffee0123456789c0ffee0123456789c0ffee0123456789c0ffee0123456789"
)

// cleanTestLayerdb는 이미지 둘(X는 레이어 셋, Y는 X와 레이어 둘을 공유), 어느 이미지도 쓰지 않는 레이어 하나, 컨테이너 하나가 있는
// graphdriver 메타데이터를 만든다. 비밀 값이 든 이미지 설정으로 읽기 범위도 확인한다.
func cleanTestLayerdb(t *testing.T, root string) {
	t.Helper()
	base := filepath.Join(root, "image", "overlay2")
	now := time.Now()
	for chain, cache := range map[string]string{layerChain1: "cacheA", layerChain2: "cacheB", layerChain3: "cacheC", "sha256:" + strings.Repeat("44", 32): "cacheU"} {
		cleanTestWriteFile(t, filepath.Join(base, "layerdb", "sha256", strings.TrimPrefix(chain, "sha256:"), "cache-id"), cache+"\n", now)
	}
	config := func(diffs ...string) string {
		quoted := make([]string, len(diffs))
		for index, diff := range diffs {
			quoted[index] = fmt.Sprintf("%q", diff)
		}
		return fmt.Sprintf(`{"config":{"Env":["SECRET=hunter2-do-not-print"]},"rootfs":{"type":"layers","diff_ids":[%s]}}`, strings.Join(quoted, ","))
	}
	cleanTestWriteFile(t, filepath.Join(base, "imagedb", "content", "sha256", layerImageX), config(layerDiff1, layerDiff2, layerDiff3), now)
	cleanTestWriteFile(t, filepath.Join(base, "imagedb", "content", "sha256", layerImageY), config(layerDiff1, layerDiff2), now)
	cleanTestWriteFile(t, filepath.Join(base, "repositories.json"), fmt.Sprintf(`{"Repositories":{"app":{"app:v1":"sha256:%s","app@sha256:%s":"sha256:%s"},"base":{"base:1":"sha256:%s"}}}`,
		layerImageX, strings.Repeat("dd", 32), layerImageX, layerImageY), now)
	cleanTestWriteFile(t, filepath.Join(base, "layerdb", "mounts", layerBoxID, "mount-id"), "mountM", now)
	cleanTestWriteFile(t, filepath.Join(base, "layerdb", "mounts", layerBoxID, "init-id"), "mountM-init", now)
}

func cleanTestOverlayContainers() []cleanOldContainer {
	return []cleanOldContainer{{Name: "web-1", ID: cleanShortID(layerBoxID)}}
}

func TestCleanAttributeOverlaySplitsTheDirectoriesByOwner(t *testing.T) {
	root := t.TempDir()
	cleanTestLayerdb(t, root)
	totals := map[string]uint64{"cacheA": 100, "cacheB": 200, "cacheC": 400, "cacheU": 50, "mountM": 30, "mountM-init": 5, "stray": 7, "l": 3}
	live := cleanDockerLive{imagesKnown: true, images: map[string]bool{"app:v1": true}}
	overlay := cleanAttributeOverlay(root, totals, cleanTestOverlayContainers(), live)
	if overlay == nil {
		t.Fatal("no attribution")
	}
	if overlay.ImageBytes != 700 || overlay.ContainerBytes != 35 || overlay.UnusedLayerBytes != 50 || overlay.UnusedLayers != 1 ||
		overlay.UnknownBytes != 7 || overlay.UnknownDirs != 1 || overlay.TotalBytes != 792 || overlay.MissingLayers != 0 {
		t.Fatalf("overlay = %+v", overlay)
	}
	if overlay.ImagesTotal != 2 || len(overlay.Images) != 2 {
		t.Fatalf("images = %+v", overlay.Images)
	}
	x, y := overlay.Images[0], overlay.Images[1]
	if x.ID != cleanShortID(layerImageX) || x.Bytes != 700 || x.SharedBytes != 300 || x.Layers != 3 || len(x.Tags) != 1 || x.Tags[0] != "app:v1" ||
		x.InEngine == nil || !*x.InEngine {
		t.Errorf("image X = %+v", x)
	}
	if y.ID != cleanShortID(layerImageY) || y.Bytes != 300 || y.SharedBytes != 300 || y.Layers != 2 || y.InEngine == nil || *y.InEngine {
		t.Errorf("image Y = %+v", y)
	}
	if len(overlay.Containers) != 1 || overlay.Containers[0].Name != "web-1" || overlay.Containers[0].Bytes != 35 {
		t.Errorf("containers = %+v", overlay.Containers)
	}
	data, err := json.Marshal(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hunter2") {
		t.Fatalf("report leaks the image environment: %s", data)
	}
}

func TestCleanAttributeOverlayWithoutTheEngineAnswerLeavesInEngineUnknown(t *testing.T) {
	root := t.TempDir()
	cleanTestLayerdb(t, root)
	overlay := cleanAttributeOverlay(root, map[string]uint64{"cacheA": 1, "cacheB": 1, "cacheC": 1}, nil, cleanDockerLive{})
	for _, image := range overlay.Images {
		if image.InEngine != nil {
			t.Errorf("image %s has in_engine %v without an engine answer", image.ID, *image.InEngine)
		}
	}
}

func TestCleanAttributeOverlayCountsLayersThatTheLayerDataLacks(t *testing.T) {
	root := t.TempDir()
	cleanTestLayerdb(t, root)
	cleanTestWriteFile(t, filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256", layerImageZ),
		fmt.Sprintf(`{"rootfs":{"diff_ids":[%q,"sha256:%s"]}}`, layerDiff1, strings.Repeat("99", 32)), time.Now())
	overlay := cleanAttributeOverlay(root, map[string]uint64{"cacheA": 10}, nil, cleanDockerLive{})
	if overlay.MissingLayers != 1 || overlay.ImagesTotal != 3 {
		t.Fatalf("missing %d images %d", overlay.MissingLayers, overlay.ImagesTotal)
	}
}

func TestCleanAttributeOverlayIsAbsentWithoutTheGraphdriverMetadata(t *testing.T) {
	root := t.TempDir()
	if got := cleanAttributeOverlay(root, map[string]uint64{"x": 1}, nil, cleanDockerLive{}); got != nil {
		t.Fatalf("overlay = %+v", got)
	}
	cleanTestLayerdb(t, root)
	if got := cleanAttributeOverlay(root, nil, nil, cleanDockerLive{}); got != nil {
		t.Fatalf("overlay without directories = %+v", got)
	}
}

func TestCleanAttributeOverlayKeepsTheLargestImagesFirstAndLimitsTheList(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "image", "overlay2")
	now := time.Now()
	totals := map[string]uint64{}
	for index := 0; index < cleanOverlayImageLimit+2; index++ {
		diff := fmt.Sprintf("sha256:%064x", index+1)
		cache := fmt.Sprintf("cache%02d", index)
		cleanTestWriteFile(t, filepath.Join(base, "layerdb", "sha256", strings.TrimPrefix(diff, "sha256:"), "cache-id"), cache, now)
		cleanTestWriteFile(t, filepath.Join(base, "imagedb", "content", "sha256", fmt.Sprintf("%064x", index+100)),
			fmt.Sprintf(`{"rootfs":{"diff_ids":[%q]}}`, diff), now)
		totals[cache] = uint64(1000 * (index + 1))
	}
	overlay := cleanAttributeOverlay(root, totals, nil, cleanDockerLive{})
	if overlay.ImagesTotal != cleanOverlayImageLimit+2 || len(overlay.Images) != cleanOverlayImageLimit || overlay.ImagesOmitted != 2 {
		t.Fatalf("total %d listed %d omitted %d", overlay.ImagesTotal, len(overlay.Images), overlay.ImagesOmitted)
	}
	if overlay.Images[0].Bytes != uint64(1000*(cleanOverlayImageLimit+2)) || overlay.Images[0].Bytes < overlay.Images[1].Bytes {
		t.Fatalf("images are not sorted by size: %+v", overlay.Images[:2])
	}
}

func TestFormatCleanOverlayShowsTheSplitAndLimitsTheImagesWithoutVerbose(t *testing.T) {
	in := true
	overlay := &cleanOverlay{ImageBytes: 7 << 30, ContainerBytes: 1 << 30, UnusedLayerBytes: 0, UnknownBytes: 0, ImagesTotal: 8, MissingLayers: 2,
		Containers: []cleanOverlayContainer{{Name: "web-1", ID: "aaa", Bytes: 1 << 30}, {ID: "bbb", Bytes: 1 << 20}}}
	for index := 0; index < 7; index++ {
		overlay.Images = append(overlay.Images, cleanOverlayImage{ID: fmt.Sprintf("id%02d", index), Tags: []string{fmt.Sprintf("app:v%d", index)},
			Bytes: uint64(7-index) << 30, SharedBytes: 1 << 20, InEngine: &in})
	}
	overlay.Images = append(overlay.Images, cleanOverlayImage{ID: "untagged", Bytes: 1})
	short := formatCleanOverlay(overlay, false, 0, cleanStyle(false))
	for _, want := range []string{"overlay2 holds: images 7.0 GB, containers 1.0 GB, unused layers 0 B, unknown folders 0 B", "app:v0", "app:v4",
		"1.0 MB shared with other images", "an image with this tag exists in the running engine", "3 more images", "2 layers of the images are missing"} {
		if !strings.Contains(short, want) {
			t.Errorf("output lacks %q:\n%s", want, short)
		}
	}
	if strings.Contains(short, "app:v5") || strings.Contains(short, "web-1") {
		t.Errorf("non-verbose output shows more than 5 images or the containers:\n%s", short)
	}
	verbose := formatCleanOverlay(overlay, true, 0, cleanStyle(false))
	for _, want := range []string{"app:v6", "image untagged (no tag)", "web-1", "bbb"} {
		if !strings.Contains(verbose, want) {
			t.Errorf("verbose output lacks %q:\n%s", want, verbose)
		}
	}
}

func TestCleanDockerRootAttributesTheMeasuredOverlayDirectories(t *testing.T) {
	root := cleanTestStaleRoot(t)
	cleanTestLayerdb(t, root)
	// cleanTestStaleRoot가 만든 overlay2/layer와 아래 stray가 알 수 없는 폴더 둘이다.
	for cache, size := range map[string]int{"cacheA": 8000, "cacheB": 16000, "cacheC": 32000, "cacheU": 4000, "mountM": 2000, "mountM-init": 1000, "stray": 500} {
		cleanTestWriteFile(t, filepath.Join(root, "overlay2", cache, "diff", "data"), strings.Repeat("o", size), time.Now())
	}
	cleanTestWriteFile(t, filepath.Join(root, "containers", layerBoxID, "config.v2.json"), `{"Name":"/web-1","Config":{"Image":"app:v1"},"State":{}}`, time.Now())
	cleanTestUseProc(t, t.TempDir())
	live := cleanDockerLive{known: true, imagesKnown: true, images: map[string]bool{"app:v1": true}}
	section := cleanDockerRootScan(context.Background(), cleanTestRootDeps(t), "linux", cleanDocker{Status: cleanMeasured, RootDir: "/live", live: live}, root)
	overlay := section.Roots[0].Overlay
	if overlay == nil {
		t.Fatalf("no overlay in %+v", section.Roots[0])
	}
	if overlay.ImageBytes == 0 || overlay.ContainerBytes == 0 || overlay.UnusedLayers != 1 || overlay.UnknownDirs != 2 || overlay.ImagesTotal != 2 {
		t.Fatalf("overlay = %+v", overlay)
	}
	if sum := overlay.ImageBytes + overlay.ContainerBytes + overlay.UnusedLayerBytes + overlay.UnknownBytes; sum != overlay.TotalBytes {
		t.Errorf("the parts add up to %d, the total is %d", sum, overlay.TotalBytes)
	}
	if overlay.ContainerBytes < 3000 || overlay.Containers[0].Name != "web-1" {
		t.Errorf("containers = %+v bytes %d", overlay.Containers, overlay.ContainerBytes)
	}
	text := formatCleanDockerRoot(section, false, 0, cleanStyle(false))
	if !strings.Contains(text, "overlay2 holds: images") || !strings.Contains(text, "app:v1") {
		t.Errorf("the section output lacks the split:\n%s", text)
	}
}
