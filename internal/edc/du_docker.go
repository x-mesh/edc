package edc

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// du는 docker 저장 경로에 container 이름을 붙인다. 디렉터리 이름은 container ID나 snapshot 번호라 어느 container의
// 것인지 읽을 수 없고, docker inspect와 mountinfo만 그 관계를 안다.

// duDockerFormat은 이름표에 쓰는 값만 읽는다. 환경 변수 같은 설정은 읽지 않는다. docker는 이 형식을 없는 키에서
// 실패하는 map 위에서 평가하므로, containerd snapshotter에서 없는 GraphDriver는 index로 읽는다. 한 container의
// template 오류는 모든 container의 출력을 멈춘다.
const duDockerFormat = `{{.Id}}|{{.Name}}|{{.LogPath}}|{{with index . "GraphDriver"}}{{with index . "Data"}}{{index . "UpperDir"}}{{end}}{{end}}|{{json .Mounts}}`

// duDockerTimeout은 docker 조회의 상한이다. 이름표는 덧붙이는 정보라 데몬이 답하지 않아도 du가 기다리지 않는다.
const duDockerTimeout = 3 * time.Second

type duDockerMount struct {
	Type   string `json:"Type"`
	Name   string `json:"Name"`
	Source string `json:"Source"`
}

// duContainerLabels는 docker inspect 출력과 mountinfo로 경로별 이름표를 만든다. 키는 절대 경로다.
// 한 경로에 이름표가 여럿이면 container 자신의 디렉터리가 volume이나 bind 원본보다 먼저다.
func duContainerLabels(inspect, mountinfo string) map[string]string {
	labels := map[string]string{}
	names := map[string]string{}
	volumes := map[string]string{}
	users := map[string][]string{}
	for _, line := range strings.Split(inspect, "\n") {
		fields := strings.SplitN(line, "|", 5)
		if len(fields) != 5 || fields[0] == "" {
			continue
		}
		id, name, logPath, upperDir := fields[0], strings.TrimPrefix(fields[1], "/"), fields[2], fields[3]
		names[id] = name
		if logPath != "" {
			labels[filepath.Dir(logPath)] = T("du.label.container", name)
		}
		// overlay2의 UpperDir는 <layer>/diff이고, 실행 중이면 <layer>/merged에 마운트된다.
		if upperDir != "" {
			layer := filepath.Dir(upperDir)
			labels[layer] = T("du.label.layer", name)
			labels[filepath.Join(layer, "merged")] = T("du.label.rootfs", name)
		}
		var mounts []duDockerMount
		if json.Unmarshal([]byte(fields[4]), &mounts) != nil {
			continue
		}
		for _, mount := range mounts {
			if mount.Source == "" {
				continue
			}
			source := filepath.Clean(mount.Source)
			// named volume의 원본은 volumes/<name>/_data다. 크기가 쌓이는 곳은 volume 디렉터리라 그쪽에 붙인다.
			if mount.Type == "volume" && filepath.Base(source) == "_data" {
				source = filepath.Dir(source)
				volumes[source] = mount.Name
			}
			if !slices.Contains(users[source], name) {
				users[source] = append(users[source], name)
			}
		}
	}
	// containerd snapshotter는 실행 중인 container의 root를 <data root>/rootfs/overlayfs/<id>에 overlay로 붙인다.
	// upperdir가 그 container의 writable layer다.
	for _, mount := range duOverlayMounts(mountinfo) {
		name, ok := names[filepath.Base(mount.point)]
		if !ok {
			continue
		}
		labels[mount.point] = T("du.label.rootfs", name)
		if mount.upper != "" {
			labels[filepath.Dir(mount.upper)] = T("du.label.layer", name)
		}
	}
	for source, containers := range users {
		if _, ok := labels[source]; ok {
			continue
		}
		sort.Strings(containers)
		if volume, ok := volumes[source]; ok {
			labels[source] = T("du.label.volume", volume, strings.Join(containers, ", "))
			continue
		}
		labels[source] = T("du.label.bind", strings.Join(containers, ", "))
	}
	return labels
}

type duOverlayMount struct{ point, upper string }

// duOverlayMounts는 mountinfo에서 overlay 마운트 지점과 upperdir를 읽는다. upperdir는 마운트 옵션에만 있어
// parseMountInfo가 버리는 칸이다.
func duOverlayMounts(mountinfo string) []duOverlayMount {
	var mounts []duOverlayMount
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		separator := -1
		for index, field := range fields {
			if field == "-" {
				separator = index
				break
			}
		}
		if separator < 5 || separator+3 >= len(fields) || fields[separator+1] != "overlay" {
			continue
		}
		mount := duOverlayMount{point: unescapeMountField(fields[4])}
		for _, option := range strings.Split(fields[separator+3], ",") {
			if upper, ok := strings.CutPrefix(option, "upperdir="); ok {
				mount.upper = unescapeMountField(upper)
			}
		}
		mounts = append(mounts, mount)
	}
	return mounts
}
