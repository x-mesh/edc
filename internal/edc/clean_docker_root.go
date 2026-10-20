package edc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 옛 Docker 데이터 디렉터리는 엔진이 data-root를 옮긴 뒤에도 남은 /var/lib/docker다. edc는 지워도 되는지 판정하지 않고,
// 사용자가 판단할 근거만 읽는다. 파일은 읽기만 하고 docker 명령도 실행하지 않는다.

const (
	cleanDockerRootDefault       = "/var/lib/docker"
	cleanReasonEngineRootUnknown = "engine_root_unknown"

	// 볼륨은 큰 순서로 이만큼만 자세히 보인다. 나머지는 개수와 크기 합만 남긴다.
	cleanDockerRootVolumeLimit = 20
	// 컨테이너 설정 파일은 환경 변수까지 담겨 커질 수 있다. 이보다 크면 읽지 않고 읽지 못한 것으로 센다.
	cleanDockerConfigMax = 4 << 20
	// 볼륨 하나에서 마지막 쓰기 시각을 찾으려고 훑는 항목의 상한이다.
	cleanDockerNewestEntries = 200000
	cleanDockerRootDirLimit  = 6
)

// 시험이 /proc을 가짜 디렉터리로 바꾼다.
var cleanProcDir = "/proc"

type cleanDockerRoots struct {
	Status  string                 `json:"status"`
	Reasons []string               `json:"reasons,omitempty"`
	Current string                 `json:"current_root,omitempty"`
	Roots   []cleanStaleDockerRoot `json:"roots,omitempty"`
}

type cleanStaleDockerRoot struct {
	Path       string  `json:"path"`
	Status     string  `json:"status"`
	Detail     string  `json:"detail,omitempty"`
	UsageBytes *uint64 `json:"usage_bytes,omitempty"`
	LowerBound bool    `json:"lower_bound,omitempty"`
	Unreadable int64   `json:"unreadable,omitempty"`
	// Cleanup은 보여 주기만 하는 명령이다. 마운트되어 있거나 열린 파일이 있으면 비어 있다.
	Cleanup string `json:"cleanup,omitempty"`
	// Mounted가 nil이면 마운트 표를 읽지 못한 것이다.
	Mounted *bool `json:"mounted,omitempty"`
	// OpenFiles가 nil이면 /proc을 읽지 못한 것이다. OpenFilesComplete가 false이면 읽지 못한 process가 있어 실제 수는 더 많을 수 있다.
	OpenFiles            *int                 `json:"open_files,omitempty"`
	OpenFilesComplete    bool                 `json:"open_files_complete"`
	LastContainerStop    *time.Time           `json:"last_container_stop,omitempty"`
	NewestVolumeWrite    *time.Time           `json:"newest_volume_write,omitempty"`
	Dirs                 []cleanDockerRootDir `json:"dirs,omitempty"`
	Containers           []cleanOldContainer  `json:"containers,omitempty"`
	ContainersUnreadable int                  `json:"containers_unreadable,omitempty"`
	Overlay              *cleanOverlay        `json:"overlay,omitempty"`
	Volumes              []cleanOldVolume     `json:"volumes,omitempty"`
	VolumesTotal         int                  `json:"volumes_total"`
	VolumesOmitted       int                  `json:"volumes_omitted,omitempty"`
	OmittedBytes         uint64               `json:"omitted_bytes,omitempty"`
}

type cleanDockerRootDir struct {
	Name  string `json:"name"`
	Bytes uint64 `json:"bytes"`
}

type cleanOldContainer struct {
	Name     string     `json:"name"`
	ID       string     `json:"id"`
	Image    string     `json:"image,omitempty"`
	Project  string     `json:"compose_project,omitempty"`
	Created  *time.Time `json:"created,omitempty"`
	Finished *time.Time `json:"finished,omitempty"`
	Running  bool       `json:"running"`
	// InEngine와 ProjectInEngine이 nil이면 엔진에 물어보지 못한 것이다.
	InEngine        *bool `json:"in_engine,omitempty"`
	ProjectInEngine *bool `json:"project_in_engine,omitempty"`
}

type cleanOldVolume struct {
	Name          string     `json:"name"`
	Anonymous     bool       `json:"anonymous"`
	Bytes         uint64     `json:"bytes"`
	NewestWrite   *time.Time `json:"newest_write,omitempty"`
	NewestPartial bool       `json:"newest_partial,omitempty"`
	UsedBy        []string   `json:"used_by,omitempty"`
	InEngine      *bool      `json:"in_engine,omitempty"`
}

// cleanDockerConfig는 컨테이너 설정 파일에서 근거로 쓰는 필드만 담는다. 환경 변수 같은 나머지 필드는 읽어도 담지 않는다.
type cleanDockerConfig struct {
	Name    string
	Created string
	Config  struct {
		Image  string
		Labels map[string]string
	}
	State struct {
		Running    bool
		FinishedAt string
	}
	MountPoints map[string]struct{ Type, Name string }
}

func cleanDockerRootScan(ctx context.Context, deps cleanDeps, goos string, docker cleanDocker, roots ...string) cleanDockerRoots {
	if goos != "linux" {
		return cleanDockerRoots{}
	}
	if err := ctx.Err(); err != nil {
		return cleanDockerRoots{Status: cleanNotScanned, Reasons: []string{cleanContextReason(err)}}
	}
	if docker.RootDir == "" {
		// docker가 없거나 엔진이 꺼져 있으면 /var/lib/docker가 옛 것인지 알 수 없고, 비교할 엔진도 없으므로 아무것도 보이지 않는다.
		if docker.Status == cleanToolAbsent || docker.Status == cleanNotRunning {
			return cleanDockerRoots{}
		}
		return cleanDockerRoots{Status: cleanUnknownLocation, Reasons: []string{cleanReasonEngineRootUnknown}}
	}
	if len(roots) == 0 {
		roots = []string{cleanDockerRootDefault}
	}
	current := cleanResolve(docker.RootDir)
	section := cleanDockerRoots{Status: cleanAbsent, Current: docker.RootDir}
	for _, root := range roots {
		resolved := cleanResolve(root)
		if resolved == current || !cleanLooksLikeDockerRoot(resolved) {
			continue
		}
		section.Roots = append(section.Roots, cleanStaleRootEvidence(ctx, deps, resolved, docker))
	}
	if len(section.Roots) == 0 {
		return section
	}
	section.Status = cleanMeasured
	if ctx.Err() != nil {
		section.Status = cleanPartial
	}
	return section
}

func cleanResolve(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

func cleanLooksLikeDockerRoot(path string) bool {
	for _, name := range []string{"overlay2", "volumes", "containers", "image"} {
		if info, err := os.Stat(filepath.Join(path, name)); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

func cleanStaleRootEvidence(ctx context.Context, deps cleanDeps, path string, docker cleanDocker) cleanStaleDockerRoot {
	root := cleanStaleDockerRoot{Path: path, Status: cleanMeasured}
	scan, err := deps.measure(ctx, duOptions{root: path, workers: duDefaultWorkers}, deps.sources)
	if err != nil {
		root.Status, root.Detail = cleanFailed, err.Error()
		return root
	}
	total := scan.root.total
	root.UsageBytes, root.LowerBound, root.Unreadable = &total, scan.partial, scan.unreadableCount
	if scan.partial {
		root.Status = cleanPartial
	}
	volumeBytes := map[string]uint64{}
	overlayBytes := map[string]uint64{}
	for _, child := range scan.root.children {
		root.Dirs = append(root.Dirs, cleanDockerRootDir{Name: child.name, Bytes: child.total})
		if child.name == "overlay2" {
			for _, layer := range child.children {
				overlayBytes[layer.name] = layer.total
			}
		}
		if child.name == "volumes" {
			for _, volume := range child.children {
				volumeBytes[volume.name] = volume.total
			}
		}
	}
	sort.Slice(root.Dirs, func(i, j int) bool { return root.Dirs[i].Bytes > root.Dirs[j].Bytes })
	if len(root.Dirs) > cleanDockerRootDirLimit {
		root.Dirs = root.Dirs[:cleanDockerRootDirLimit]
	}
	usedBy := cleanReadOldContainers(path, docker, &root)
	root.Overlay = cleanAttributeOverlay(path, overlayBytes, root.Containers, docker.live)
	cleanReadOldVolumes(ctx, path, docker, volumeBytes, usedBy, &root)
	root.Mounted = cleanMountedUnder(deps, path)
	count, complete, readable := cleanOpenUnder(ctx, cleanProcDir, path)
	root.OpenFilesComplete = complete
	if readable {
		root.OpenFiles = &count
	}
	// 볼륨 하나씩 지우는 명령은 두지 않는다. docker volume rm은 이 디렉터리가 아니라 지금 엔진의 볼륨을 지운다.
	// 쓰는 중이 아니라고 확인한 경우에만 명령을 둔다. 마운트 표나 /proc을 읽지 못했거나 읽지 못한 process가 있으면
	// 열린 파일이 0으로 보여도 쓰는 중일 수 있어 명령을 두지 않는다.
	if root.Mounted != nil && !*root.Mounted && root.OpenFiles != nil && *root.OpenFiles == 0 && complete {
		root.Cleanup = "rm -rf --one-file-system " + cleanShellQuote(path)
	}
	return root
}

// cleanShellQuote는 셸이 한 단어로 읽도록 필요할 때만 작은따옴표로 감싼다.
func cleanShellQuote(text string) string {
	safe := text != ""
	for _, char := range text {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("_./-", char)) {
			safe = false
			break
		}
	}
	if safe {
		return text
	}
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}

// cleanReadOldContainers는 containers/*/config.v2.json에서 컨테이너 기록을 읽고, 볼륨 이름마다 그것을 쓴 컨테이너 이름을 돌려준다.
func cleanReadOldContainers(path string, docker cleanDocker, root *cleanStaleDockerRoot) map[string][]string {
	usedBy := map[string][]string{}
	entries, err := os.ReadDir(filepath.Join(path, "containers"))
	if err != nil {
		return usedBy
	}
	liveProjects := map[string]bool{}
	for _, project := range docker.live.containers {
		liveProjects[project] = true
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		config, ok := cleanReadDockerConfig(filepath.Join(path, "containers", entry.Name(), "config.v2.json"))
		if !ok {
			root.ContainersUnreadable++
			continue
		}
		name := strings.TrimPrefix(config.Name, "/")
		container := cleanOldContainer{Name: name, ID: cleanShortID(entry.Name()), Image: config.Config.Image,
			Project: config.Config.Labels["com.docker.compose.project"], Created: cleanDockerTime(config.Created),
			Finished: cleanDockerTime(config.State.FinishedAt), Running: config.State.Running}
		if docker.live.known {
			_, inEngine := docker.live.containers[name]
			container.InEngine = &inEngine
			if container.Project != "" {
				projectInEngine := liveProjects[container.Project]
				container.ProjectInEngine = &projectInEngine
			}
		}
		if container.Finished != nil && !container.Running && (root.LastContainerStop == nil || container.Finished.After(*root.LastContainerStop)) {
			root.LastContainerStop = container.Finished
		}
		root.Containers = append(root.Containers, container)
		for _, mount := range config.MountPoints {
			if mount.Name != "" && (mount.Type == "volume" || mount.Type == "") {
				usedBy[mount.Name] = append(usedBy[mount.Name], name)
			}
		}
	}
	sort.Slice(root.Containers, func(i, j int) bool { return root.Containers[i].Name < root.Containers[j].Name })
	return usedBy
}

func cleanReadDockerConfig(path string) (cleanDockerConfig, bool) {
	var config cleanDockerConfig
	info, err := os.Stat(path)
	if err != nil || info.Size() > cleanDockerConfigMax {
		return config, false
	}
	file, err := os.Open(path)
	if err != nil {
		return config, false
	}
	defer file.Close()
	if err := json.NewDecoder(io.LimitReader(file, cleanDockerConfigMax)).Decode(&config); err != nil {
		return config, false
	}
	return config, true
}

func cleanReadOldVolumes(ctx context.Context, path string, docker cleanDocker, bytes map[string]uint64, usedBy map[string][]string, root *cleanStaleDockerRoot) {
	entries, err := os.ReadDir(filepath.Join(path, "volumes"))
	if err != nil {
		return
	}
	var volumes []cleanOldVolume
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		volume := cleanOldVolume{Name: entry.Name(), Anonymous: cleanAnonymousVolume(entry.Name()), Bytes: bytes[entry.Name()],
			UsedBy: usedBy[entry.Name()]}
		if docker.live.known {
			inEngine := docker.live.volumes[entry.Name()]
			volume.InEngine = &inEngine
		}
		volumes = append(volumes, volume)
	}
	sort.Slice(volumes, func(i, j int) bool {
		if volumes[i].Bytes != volumes[j].Bytes {
			return volumes[i].Bytes > volumes[j].Bytes
		}
		return volumes[i].Name < volumes[j].Name
	})
	root.VolumesTotal = len(volumes)
	for index := range volumes {
		if index >= cleanDockerRootVolumeLimit {
			root.VolumesOmitted++
			root.OmittedBytes += volumes[index].Bytes
			continue
		}
		newest, partial := cleanNewestWrite(ctx, filepath.Join(path, "volumes", volumes[index].Name))
		if !newest.IsZero() {
			utc := newest.UTC()
			volumes[index].NewestWrite = &utc
			if root.NewestVolumeWrite == nil || utc.After(*root.NewestVolumeWrite) {
				root.NewestVolumeWrite = &utc
			}
		}
		volumes[index].NewestPartial = partial
		root.Volumes = append(root.Volumes, volumes[index])
	}
}

// cleanNewestWrite는 dir 아래 일반 파일의 수정 시각 가운데 가장 늦은 것을 돌려준다. 항목이 상한을 넘거나 ctx가 끝나면 partial이다.
func cleanNewestWrite(ctx context.Context, dir string) (newest time.Time, partial bool) {
	seen := 0
	_ = filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			partial = true
			return fs.SkipAll
		}
		if err != nil {
			// 읽지 못한 항목에 더 늦은 파일이 있을 수 있어 가장 최근 쓰기를 완전한 값으로 보고하지 않는다.
			partial = true
			return nil
		}
		if seen++; seen > cleanDockerNewestEntries {
			partial = true
			return fs.SkipAll
		}
		// 디렉터리의 수정 시각은 항목을 만들거나 지운 때라 볼륨을 만든 때로 끌려간다. 파일만 본다.
		if !entry.Type().IsRegular() {
			return nil
		}
		if info, err := entry.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest, partial
}

// cleanMountedUnder는 root 자신이나 그 아래에 마운트가 있는지 본다. 마운트 표를 읽지 못하면 nil이다.
func cleanMountedUnder(deps cleanDeps, root string) *bool {
	if deps.sources.mountinfo == nil {
		return nil
	}
	text, err := deps.sources.mountinfo()
	if err != nil {
		return nil
	}
	mounted := false
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if point := fields[4]; point == root || strings.HasPrefix(point, root+"/") {
			mounted = true
		}
	}
	return &mounted
}

// cleanOpenUnder는 root 아래 파일을 열어 둔 fd 수를 센다. 다른 사용자의 process를 읽지 못하면 complete가 false이고,
// /proc 자체를 읽지 못하면 readable이 false다.
func cleanOpenUnder(ctx context.Context, procDir, root string) (count int, complete, readable bool) {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return 0, false, false
	}
	complete = true
	prefix := root + "/"
	for _, entry := range entries {
		if ctx.Err() != nil {
			return count, false, true
		}
		if !entry.IsDir() || !cleanNumeric(entry.Name()) {
			continue
		}
		fdDir := filepath.Join(procDir, entry.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			// process가 읽는 사이에 끝난 것은 읽지 못한 것이 아니다.
			if !errors.Is(err, fs.ErrNotExist) {
				complete = false
			}
			continue
		}
		for _, fd := range fds {
			if target, err := os.Readlink(filepath.Join(fdDir, fd.Name())); err == nil && (target == root || strings.HasPrefix(target, prefix)) {
				count++
			}
		}
	}
	return count, complete, true
}

func cleanNumeric(text string) bool {
	if text == "" {
		return false
	}
	for _, char := range text {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// cleanAnonymousVolume은 docker가 이름 없이 만든 볼륨의 이름인 64자리 16진수인지 본다.
func cleanAnonymousVolume(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, char := range name {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func cleanShortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// cleanDockerTime은 docker가 기록한 시각을 읽는다. 시작한 적 없는 컨테이너의 0001-01-01은 시각이 없는 것으로 본다.
func cleanDockerTime(text string) *time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || parsed.Year() < 2000 {
		return nil
	}
	utc := parsed.UTC()
	return &utc
}

const cleanDockerRootDateLayout = "2006-01-02"

func cleanDockerDate(at *time.Time) string {
	return at.Format(cleanDockerRootDateLayout)
}

func formatCleanDockerRoot(section cleanDockerRoots, verbose bool, width int, style cleanStyle) string {
	switch section.Status {
	case "", cleanAbsent:
		return ""
	case cleanMeasured, cleanPartial:
	default:
		return duFit("\n"+style.state(T("clean.docker_root.state", cleanStateText(section.Status, section.Reasons)), section.Status), width) + "\n"
	}
	var builder strings.Builder
	for _, root := range section.Roots {
		builder.WriteString(formatCleanStaleRoot(root, section.Current, verbose, width, style))
	}
	return builder.String()
}

func formatCleanStaleRoot(root cleanStaleDockerRoot, current string, verbose bool, width int, style cleanStyle) string {
	var builder strings.Builder
	line := func(indent, text string) { builder.WriteString(style.row(indent, text, width)) }
	builder.WriteString(style.open(T("clean.docker_root.title", root.Path), T("clean.docker_root.panel"), T("clean.docker_root.note"), width))
	if root.UsageBytes == nil {
		line("  ", style.state(cleanStateText(root.Status, nil), root.Status)+" "+root.Detail)
		builder.WriteString(style.close())
		return builder.String()
	}
	size := formatSizeShort(*root.UsageBytes)
	if root.LowerBound {
		size = "≥ " + size
	}
	line("  ", style.size(size, *root.UsageBytes, true)+"  "+root.Path)
	indent := "  "
	signals := []string{T("clean.docker_root.engine_root", current)}
	switch {
	case root.Mounted == nil:
		signals = append(signals, T("clean.docker_root.mounted_unknown"))
	case *root.Mounted:
		signals = append(signals, style.warn(T("clean.docker_root.mounted")))
	default:
		signals = append(signals, T("clean.docker_root.not_mounted"))
	}
	switch {
	case root.OpenFiles == nil:
		signals = append(signals, style.warn(T("clean.docker_root.open_unknown")))
	case *root.OpenFiles > 0:
		signals = append(signals, style.warn(T("clean.docker_root.open_some", *root.OpenFiles)))
	case root.OpenFilesComplete:
		signals = append(signals, T("clean.docker_root.open_none"))
	default:
		signals = append(signals, style.warn(T("clean.docker_root.open_unreadable")))
	}
	for _, signal := range signals {
		line(indent, style.dim("· ")+signal)
	}
	if root.LastContainerStop != nil {
		line(indent, style.dim("· ")+T("clean.docker_root.last_stop", cleanDockerDate(root.LastContainerStop)))
	}
	if root.NewestVolumeWrite != nil {
		line(indent, style.dim("· ")+T("clean.docker_root.newest_write", cleanDockerDate(root.NewestVolumeWrite)))
	}
	if root.Unreadable > 0 {
		line(indent, style.warn("· "+T("clean.docker_root.unreadable", root.Unreadable)))
	}
	for _, dir := range root.Dirs {
		line("    ", style.size(cleanPadLeft(formatSizeShort(dir.Bytes), 9), dir.Bytes, false)+"  "+dir.Name)
	}
	if root.Overlay != nil {
		builder.WriteString(formatCleanOverlay(root.Overlay, verbose, width, style))
	}
	if root.VolumesTotal > 0 {
		line(indent, T("clean.docker_root.volumes", root.VolumesTotal))
	}
	var anonymous int
	var anonymousBytes uint64
	for _, volume := range root.Volumes {
		if volume.Anonymous && !verbose {
			anonymous++
			anonymousBytes += volume.Bytes
			continue
		}
		line("    ", style.size(cleanPadLeft(formatSizeShort(volume.Bytes), 9), volume.Bytes, false)+"  "+volume.Name)
		for _, detail := range cleanVolumeDetails(volume) {
			line("               ", style.dim(detail))
		}
	}
	if anonymous > 0 {
		line("    ", style.dim(T("clean.docker_root.anonymous", anonymous, formatSizeShort(anonymousBytes))))
	}
	if root.VolumesOmitted > 0 {
		line("    ", style.dim(T("clean.docker_root.omitted", root.VolumesOmitted, formatSizeShort(root.OmittedBytes))))
	}
	if len(root.Containers) > 0 {
		running := 0
		for _, container := range root.Containers {
			if container.Running {
				running++
			}
		}
		hint := ""
		if !verbose {
			hint = " " + T("clean.docker_root.containers_hint")
		}
		line(indent, T("clean.docker_root.containers", len(root.Containers), running)+style.dim(hint))
		if verbose {
			for _, container := range root.Containers {
				line("    ", container.Name+style.dim("  "+strings.Join(cleanContainerDetails(container), " · ")))
			}
		}
	}
	if verbose && root.Cleanup != "" {
		line(indent, style.phrase("clean.cleanup", root.Cleanup, richCyan))
	}
	builder.WriteString(style.close())
	return builder.String()
}

func cleanVolumeDetails(volume cleanOldVolume) []string {
	var details []string
	switch len(volume.UsedBy) {
	case 0:
		details = append(details, T("clean.docker_root.no_record"))
	default:
		details = append(details, T("clean.docker_root.used_by", strings.Join(volume.UsedBy, ", ")))
	}
	if volume.NewestWrite != nil {
		text := T("clean.docker_root.newest", cleanDockerDate(volume.NewestWrite))
		if volume.NewestPartial {
			text = "≥ " + text
		}
		details = append(details, text)
	}
	if volume.InEngine != nil && *volume.InEngine {
		details = append(details, T("clean.docker_root.volume_in_engine"))
	}
	return details
}

func cleanContainerDetails(container cleanOldContainer) []string {
	var details []string
	if container.Image != "" {
		details = append(details, container.Image)
	}
	if container.Project != "" {
		details = append(details, T("clean.docker_root.project", container.Project))
	}
	switch {
	case container.Running:
		details = append(details, T("clean.docker_root.recorded_running"))
	case container.Finished != nil:
		details = append(details, T("clean.docker_root.stopped", cleanDockerDate(container.Finished)))
	}
	if container.InEngine != nil && *container.InEngine {
		details = append(details, T("clean.docker_root.container_in_engine"))
	}
	if container.ProjectInEngine != nil && *container.ProjectInEngine {
		details = append(details, T("clean.docker_root.project_in_engine"))
	}
	return details
}

// formatCleanOverlay는 overlay2의 크기를 이미지와 컨테이너로 나눈 내역이다. -v가 아니면 큰 이미지 5개까지만 보인다.
func formatCleanOverlay(overlay *cleanOverlay, verbose bool, width int, style cleanStyle) string {
	var builder strings.Builder
	line := func(indent, text string) { builder.WriteString(style.row(indent, text, width)) }
	line("  ", T("clean.docker_root.overlay", formatSizeShort(overlay.ImageBytes), formatSizeShort(overlay.ContainerBytes),
		formatSizeShort(overlay.UnusedLayerBytes), formatSizeShort(overlay.UnknownBytes)))
	limit := 5
	if verbose {
		limit = len(overlay.Images)
	}
	shown := min(limit, len(overlay.Images))
	for _, image := range overlay.Images[:shown] {
		label := strings.Join(image.Tags, ", ")
		if label == "" {
			label = T("clean.docker_root.overlay_no_tag", image.ID)
		}
		line("    ", style.size(cleanPadLeft(formatSizeShort(image.Bytes), 9), image.Bytes, false)+"  "+label)
		if image.SharedBytes > 0 {
			line("               ", style.dim(T("clean.docker_root.overlay_shared", formatSizeShort(image.SharedBytes))))
		}
		if image.InEngine != nil && *image.InEngine {
			line("               ", style.dim(T("clean.docker_root.overlay_in_engine")))
		}
	}
	if hidden := overlay.ImagesTotal - shown; hidden > 0 {
		line("    ", style.dim(T("clean.docker_root.overlay_more", hidden)))
	}
	if verbose {
		for _, container := range overlay.Containers {
			name := container.Name
			if name == "" {
				name = container.ID
			}
			line("    ", style.size(cleanPadLeft(formatSizeShort(container.Bytes), 9), container.Bytes, false)+"  "+name)
		}
	}
	if overlay.MissingLayers > 0 {
		line("  ", style.warn(T("clean.docker_root.overlay_missing", overlay.MissingLayers)))
	}
	return builder.String()
}
