package edc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 옛 Docker 데이터의 overlay2 폴더는 이름만으로는 무엇인지 알 수 없다. graphdriver가 남긴 메타데이터로 폴더마다 주인을 찾는다.
// layerdb/sha256/<chainID>/cache-id는 레이어의 overlay2 폴더 이름이고, layerdb/mounts/<컨테이너 ID>/의 mount-id와 init-id는
// 컨테이너의 쓰기 레이어 폴더다. 이미지는 imagedb의 rootfs.diff_ids에서 chainID를 계산해 레이어를 찾는다. 파일은 읽기만 한다.

const (
	cleanOverlayImageLimit = 10
	cleanLayerFileMax      = 1 << 20
	cleanImageConfigMax    = 8 << 20
)

type cleanOverlayImage struct {
	ID   string   `json:"id"`
	Tags []string `json:"tags,omitempty"`
	// Layers는 이미지가 쓰는 레이어 수이고, Bytes는 그 레이어 폴더의 합이다. SharedBytes는 다른 이미지도 쓰는 레이어의 합이라
	// 이 이미지를 지워도 비워지지 않는 양이다.
	Layers      int    `json:"layers"`
	Bytes       uint64 `json:"bytes"`
	SharedBytes uint64 `json:"shared_bytes,omitempty"`
	// InEngine이 nil이면 엔진에 물어보지 못한 것이다.
	InEngine *bool `json:"in_engine,omitempty"`
}

type cleanOverlayContainer struct {
	Name  string `json:"name"`
	ID    string `json:"id"`
	Bytes uint64 `json:"bytes"`
}

type cleanOverlay struct {
	TotalBytes       uint64 `json:"total_bytes"`
	ImageBytes       uint64 `json:"image_bytes"`
	ContainerBytes   uint64 `json:"container_bytes"`
	UnusedLayerBytes uint64 `json:"unused_layer_bytes"`
	UnusedLayers     int    `json:"unused_layers"`
	UnknownBytes     uint64 `json:"unknown_bytes"`
	UnknownDirs      int    `json:"unknown_dirs"`
	// MissingLayers는 이미지가 가리키지만 layerdb에 없는 레이어 수다.
	MissingLayers int                     `json:"missing_layers,omitempty"`
	Images        []cleanOverlayImage     `json:"images,omitempty"`
	ImagesTotal   int                     `json:"images_total"`
	ImagesOmitted int                     `json:"images_omitted,omitempty"`
	Containers    []cleanOverlayContainer `json:"containers,omitempty"`
}

// cleanAttributeOverlay는 overlay2 폴더의 크기(totals)를 이미지와 컨테이너로 나눈다. layerdb가 없는 저장소 형식이면 nil이다.
func cleanAttributeOverlay(path string, totals map[string]uint64, containers []cleanOldContainer, live cleanDockerLive) *cleanOverlay {
	base := filepath.Join(path, "image", "overlay2")
	entries, err := os.ReadDir(filepath.Join(base, "layerdb", "sha256"))
	if err != nil || len(totals) == 0 {
		return nil
	}
	cacheOf := map[string]string{} // chainID -> overlay2 폴더 이름
	layerDirs := map[string]bool{}
	for _, entry := range entries {
		if cacheID, ok := cleanReadTrimmed(filepath.Join(base, "layerdb", "sha256", entry.Name(), "cache-id")); ok {
			cacheOf["sha256:"+entry.Name()] = cacheID
			layerDirs[cacheID] = true
		}
	}
	overlay := &cleanOverlay{}
	containerDirs := map[string]bool{}
	names := map[string]string{}
	for _, container := range containers {
		names[container.ID] = container.Name
	}
	mounts, _ := os.ReadDir(filepath.Join(base, "layerdb", "mounts"))
	for _, mount := range mounts {
		var bytes uint64
		for _, file := range []string{"mount-id", "init-id"} {
			if id, ok := cleanReadTrimmed(filepath.Join(base, "layerdb", "mounts", mount.Name(), file)); ok {
				containerDirs[id] = true
				bytes += totals[id]
			}
		}
		overlay.ContainerBytes += bytes
		overlay.Containers = append(overlay.Containers, cleanOverlayContainer{Name: names[cleanShortID(mount.Name())], ID: cleanShortID(mount.Name()), Bytes: bytes})
	}
	sort.Slice(overlay.Containers, func(i, j int) bool { return overlay.Containers[i].Bytes > overlay.Containers[j].Bytes })

	tags := cleanImageTags(base)
	images := cleanReadImages(base, cacheOf, tags, overlay)
	users := map[string]int{}
	for _, image := range images {
		for _, cacheID := range image.layers {
			users[cacheID]++
		}
	}
	for cacheID := range users {
		overlay.ImageBytes += totals[cacheID]
	}
	result := make([]cleanOverlayImage, 0, len(images))
	for _, image := range images {
		entry := cleanOverlayImage{ID: image.id, Tags: image.tags, Layers: len(image.layers)}
		for _, cacheID := range image.layers {
			entry.Bytes += totals[cacheID]
			if users[cacheID] > 1 {
				entry.SharedBytes += totals[cacheID]
			}
		}
		if live.imagesKnown {
			inEngine := false
			for _, tag := range image.tags {
				inEngine = inEngine || live.images[tag]
			}
			entry.InEngine = &inEngine
		}
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i].Bytes-result[i].SharedBytes, result[j].Bytes-result[j].SharedBytes
		if left != right {
			return left > right
		}
		return result[i].ID < result[j].ID
	})
	overlay.ImagesTotal = len(result)
	if len(result) > cleanOverlayImageLimit {
		overlay.ImagesOmitted = len(result) - cleanOverlayImageLimit
		result = result[:cleanOverlayImageLimit]
	}
	overlay.Images = result

	for name, bytes := range totals {
		if name == "l" {
			continue // 짧은 이름의 심볼릭 링크 폴더다. 데이터가 아니다.
		}
		overlay.TotalBytes += bytes
		switch {
		case users[name] > 0 || containerDirs[name]:
		case layerDirs[name]:
			overlay.UnusedLayerBytes += bytes
			overlay.UnusedLayers++
		default:
			overlay.UnknownBytes += bytes
			overlay.UnknownDirs++
		}
	}
	return overlay
}

type cleanImageLayers struct {
	id     string
	tags   []string
	layers []string // overlay2 폴더 이름, 중복 없이
}

// cleanReadImages는 imagedb의 이미지마다 rootfs.diff_ids에서 chainID를 계산해 overlay2 폴더를 찾는다.
// 이미지 설정의 나머지 필드(환경 변수 등)는 읽어도 담지 않는다.
func cleanReadImages(base string, cacheOf map[string]string, tags map[string][]string, overlay *cleanOverlay) []cleanImageLayers {
	entries, err := os.ReadDir(filepath.Join(base, "imagedb", "content", "sha256"))
	if err != nil {
		return nil
	}
	var images []cleanImageLayers
	for _, entry := range entries {
		var config struct {
			RootFS struct {
				DiffIDs []string `json:"diff_ids"`
			} `json:"rootfs"`
		}
		data, ok := cleanReadLimited(filepath.Join(base, "imagedb", "content", "sha256", entry.Name()), cleanImageConfigMax)
		if !ok || json.Unmarshal([]byte(data), &config) != nil {
			continue
		}
		image := cleanImageLayers{id: cleanShortID(entry.Name()), tags: tags[entry.Name()]}
		seen := map[string]bool{}
		chain := ""
		for index, diffID := range config.RootFS.DiffIDs {
			if index == 0 {
				chain = diffID
			} else {
				sum := sha256.Sum256([]byte(chain + " " + diffID))
				chain = "sha256:" + hex.EncodeToString(sum[:])
			}
			cacheID, found := cacheOf[chain]
			switch {
			case !found:
				overlay.MissingLayers++
			case !seen[cacheID]:
				seen[cacheID] = true
				image.layers = append(image.layers, cacheID)
			}
		}
		images = append(images, image)
	}
	return images
}

// cleanImageTags는 repositories.json에서 이미지 ID마다 태그를 모은다. @sha256 꼴의 digest 이름은 태그가 아니라서 뺀다.
func cleanImageTags(base string) map[string][]string {
	tags := map[string][]string{}
	text, ok := cleanReadLimited(filepath.Join(base, "repositories.json"), cleanImageConfigMax)
	if !ok {
		return tags
	}
	var repositories struct {
		Repositories map[string]map[string]string `json:"Repositories"`
	}
	if json.Unmarshal([]byte(text), &repositories) != nil {
		return tags
	}
	for _, references := range repositories.Repositories {
		for reference, imageID := range references {
			if strings.Contains(reference, "@") {
				continue
			}
			id := strings.TrimPrefix(imageID, "sha256:")
			tags[id] = append(tags[id], reference)
		}
	}
	for id := range tags {
		sort.Strings(tags[id])
	}
	return tags
}

func cleanReadTrimmed(path string) (string, bool) {
	text, ok := cleanReadLimited(path, cleanLayerFileMax)
	if !ok {
		return "", false
	}
	text = strings.TrimSpace(text)
	return text, text != ""
}
