package edc

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// 이미지 목록과 detach 계획이 결과의 본문이므로 -v 없이도 보인다. Linux의 disk.check는 층 목록이 길어 그대로 둔다.
func init() {
	evidenceShownProbes[diskProbeImages] = true
	evidenceShownProbes[diskProbeDetach] = true
}

const (
	diskProbeImages = "disk.images"
	diskProbeDetach = "disk.detach"
	diskImageTool   = "/usr/bin/hdiutil"
	// diskImageVolumes는 사용자가 연 이미지가 붙는 곳이다. Xcode simulator runtime과 cryptex 같은 시스템 이미지는
	// 다른 경로에 붙고, 떼면 그 기능이 멈추므로 대상에서 뺀다.
	diskImageVolumes = "/Volumes/"
	// diskImageReadTimeout은 info가 hdiutil을 기다리는 상한이다. 측정한 실행 시간은 15 ms 안팎이었다.
	diskImageReadTimeout = 3 * time.Second
)

// diskImage는 hdiutil이 붙인 이미지 하나다. Device는 첫 system entity인 전체 디스크이고, detach는 이것으로 한다.
type diskImage struct {
	Path     string
	Device   string
	Devices  []string
	Mounts   []string
	Writable bool
	Size     uint64
}

// leftover는 설치용 DMG처럼 열어 두고 잊은 이미지다. 쓸 수 있는 이미지는 사용자가 저장소로 쓰는 것일 수 있어 뺀다.
func (image diskImage) leftover() bool {
	return !image.Writable && image.volumeMount() != ""
}

func (image diskImage) volumeMount() string {
	for _, mount := range image.Mounts {
		if strings.HasPrefix(mount, diskImageVolumes) {
			return mount
		}
	}
	return ""
}

// diskImagesSupported는 hdiutil이 있는 OS다.
func diskImagesSupported() bool { return runtime.GOOS == "darwin" }

func readDiskImages(ctx context.Context) ([]diskImage, error) {
	stdout, stderr, err := runDiskImageTool(ctx, "info", "-plist")
	if err != nil {
		return nil, diskImageToolError("hdiutil info", stdout, stderr, err)
	}
	return parseDiskImages([]byte(stdout))
}

func runDiskImageTool(ctx context.Context, args ...string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, diskImageTool, args...)
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func diskImageToolError(command, stdout, stderr string, err error) error {
	if message := diskCommandMessage(stderr, stdout); message != "" {
		return fmt.Errorf("%s: %s", command, message)
	}
	return fmt.Errorf("%s: %w", command, err)
}

// leftoverDiskImages는 info가 숨기고 detach가 떼는 이미지다. 두 명령이 같은 규칙을 쓰도록 한 곳에 둔다.
func leftoverDiskImages(images []diskImage) []diskImage {
	var result []diskImage
	for _, image := range images {
		if image.leftover() {
			result = append(result, image)
		}
	}
	return result
}

// markDiskImages는 df 행에 이미지 경로를 붙인다. 경로 표기가 달라도 놓치지 않도록 마운트와 장치 둘 다로 맞춘다.
func markDiskImages(disks []diskDetails, images []diskImage) {
	byMount, byDevice := map[string]string{}, map[string]string{}
	for _, image := range leftoverDiskImages(images) {
		for _, mount := range image.Mounts {
			byMount[mount] = image.Path
		}
		for _, device := range image.Devices {
			byDevice[device] = image.Path
		}
	}
	for index, disk := range disks {
		if path, ok := byMount[disk.Mount]; ok {
			disks[index].Image = path
		} else if path, ok := byDevice[disk.Device]; ok {
			disks[index].Image = path
		}
	}
}

func parseDiskImages(data []byte) ([]diskImage, error) {
	root, err := parsePlist(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("hdiutil info: %w", err)
	}
	top, ok := root.(map[string]any)
	if !ok {
		return nil, errors.New("hdiutil info: top level is not a dict")
	}
	items, _ := top["images"].([]any)
	images := make([]diskImage, 0, len(items))
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}
		image := diskImage{}
		image.Path, _ = fields["image-path"].(string)
		image.Writable, _ = fields["writeable"].(bool)
		blocks, _ := fields["blockcount"].(int64)
		size, _ := fields["blocksize"].(int64)
		if blocks > 0 && size > 0 {
			image.Size = uint64(blocks) * uint64(size)
		}
		entities, _ := fields["system-entities"].([]any)
		for _, entity := range entities {
			values, ok := entity.(map[string]any)
			if !ok {
				continue
			}
			if device, _ := values["dev-entry"].(string); device != "" {
				image.Devices = append(image.Devices, device)
				if image.Device == "" {
					image.Device = device
				}
			}
			if mount, _ := values["mount-point"].(string); mount != "" {
				image.Mounts = append(image.Mounts, mount)
			}
		}
		if image.Device != "" {
			images = append(images, image)
		}
	}
	return images, nil
}

// parsePlist는 XML plist를 Go 값으로 읽는다. hdiutil이 쓰는 dict, array, string, integer, bool만 다룬다.
func parsePlist(reader io.Reader) (any, error) {
	decoder := xml.NewDecoder(reader)
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Local != "plist" {
			return parsePlistValue(decoder, start)
		}
	}
}

func parsePlistValue(decoder *xml.Decoder, start xml.StartElement) (any, error) {
	switch start.Name.Local {
	case "dict":
		result := map[string]any{}
		key := ""
		for {
			token, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			switch element := token.(type) {
			case xml.EndElement:
				return result, nil
			case xml.StartElement:
				if element.Name.Local == "key" {
					if err := decoder.DecodeElement(&key, &element); err != nil {
						return nil, err
					}
					continue
				}
				value, err := parsePlistValue(decoder, element)
				if err != nil {
					return nil, err
				}
				result[key] = value
			}
		}
	case "array":
		var result []any
		for {
			token, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			switch element := token.(type) {
			case xml.EndElement:
				return result, nil
			case xml.StartElement:
				value, err := parsePlistValue(decoder, element)
				if err != nil {
					return nil, err
				}
				result = append(result, value)
			}
		}
	case "true", "false":
		if err := decoder.Skip(); err != nil {
			return nil, err
		}
		return start.Name.Local == "true", nil
	}
	var text string
	if err := decoder.DecodeElement(&text, &start); err != nil {
		return nil, err
	}
	if start.Name.Local == "integer" {
		return strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	}
	return text, nil
}

// checkDiskImages는 macOS의 disk check다. 떼어도 되는 이미지를 원본 파일과 함께 보인다.
func checkDiskImages(images []diskImage, path string) Result {
	started := time.Now()
	targets, err := selectDiskImages(images, filterArgs(path))
	if err != nil {
		// check는 읽기만 하므로 이미지가 아닌 마운트를 받아도 실패로 끝내지 않는다. 예전에는 macOS에서 skip이었다.
		return unsupported(diskProbeImages, err.Error())
	}
	result := Result{Probe: diskProbeImages, Status: StatusPass, StartedAt: started.UTC(), Summary: T("disk.image.check.none")}
	if len(targets) > 0 {
		result.Summary = T("disk.image.check.found", len(targets))
		result.Evidence = diskImageEvidence(targets)
		result.Next = "edc disk detach"
	}
	return finishDiskResult(result, started)
}

func filterArgs(path string) []string {
	if path == "" {
		return nil
	}
	return []string{path}
}

// selectDiskImages는 인자로 받은 마운트만 고른다. 남은 이미지가 아닌 마운트를 받으면 떼지 않고 실패한다.
func selectDiskImages(images []diskImage, mounts []string) ([]diskImage, error) {
	leftover := leftoverDiskImages(images)
	if len(mounts) == 0 {
		return leftover, nil
	}
	var result []diskImage
	for _, mount := range mounts {
		if len(mount) > 1 {
			mount = strings.TrimSuffix(mount, "/")
		}
		found := false
		for _, image := range leftover {
			if image.volumeMount() == mount {
				result = append(result, image)
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New(T("disk.image.error.not_image", mount))
		}
	}
	return result, nil
}

func diskImageEvidence(images []diskImage) []Evidence {
	evidence := make([]Evidence, 0, len(images))
	for _, image := range images {
		evidence = append(evidence, Evidence{Label: image.volumeMount(), Value: fmt.Sprintf("%s · %s", image.Path, formatBytes(image.Size))})
	}
	return evidence
}

func diskDetachCommand(image diskImage) []string { return []string{"hdiutil", "detach", image.Device} }

type diskDetachInput struct {
	mounts  []string
	dryRun  bool
	confirm func(detail, question string) (bool, error)
	run     func(ctx context.Context, args ...string) (string, string, error)
}

// executeDiskDetach는 계획 → 확인 → 이미지마다 detach 순서로 돈다. 하나가 실패해도 나머지는 뗀다.
// -force는 쓰지 않는다. 열린 파일이 있는 이미지는 hdiutil이 거절하는 대로 실패로 보인다.
func executeDiskDetach(ctx context.Context, images []diskImage, input diskDetachInput) diskGrowOutcome {
	started := time.Now()
	targets, err := selectDiskImages(images, input.mounts)
	if err != nil {
		return diskGrowOutcome{Result: resultFromError(diskProbeDetach, started, "disk", err)}
	}
	result := Result{Probe: diskProbeDetach, Status: StatusPass, StartedAt: started.UTC()}
	if len(targets) == 0 {
		result.Summary = T("disk.image.check.none")
		return diskGrowOutcome{Result: finishDiskResult(result, started)}
	}
	plan := make([]Evidence, 0, len(targets))
	for _, image := range targets {
		plan = append(plan, Evidence{Label: image.volumeMount(), Value: strings.Join(diskDetachCommand(image), " ") + " · " + image.Path})
	}
	if input.dryRun {
		result.Summary, result.Evidence = T("disk.image.detach.dry_run", len(targets)), plan
		return diskGrowOutcome{Result: finishDiskResult(result, started)}
	}
	confirmed, err := input.confirm(diskDetachDetail(plan), T("disk.image.detach.question", len(targets)))
	if err != nil {
		return diskGrowOutcome{Result: resultFromError(diskProbeDetach, started, "confirm", err)}
	}
	if !confirmed {
		return diskGrowOutcome{Cancelled: true}
	}
	// detach는 시간 제한으로 끊지 않는다. 떼는 도중에 끊으면 이미지가 붙었는지 알 수 없게 된다.
	changeCtx := context.WithoutCancel(ctx)
	detached := 0
	for _, image := range targets {
		command := diskDetachCommand(image)
		stdout, stderr, err := input.run(changeCtx, command[1:]...)
		if err != nil {
			result.Evidence = append(result.Evidence, Evidence{Label: image.volumeMount(), Value: T("disk.image.detach.failed", diskImageToolError(strings.Join(command, " "), stdout, stderr, err))})
			continue
		}
		detached++
		result.Evidence = append(result.Evidence, Evidence{Label: image.volumeMount(), Value: T("disk.image.detach.done") + " · " + image.Path})
	}
	result.Summary = T("disk.image.detach.summary", detached, len(targets))
	if detached < len(targets) {
		result.Status = StatusFail
		result.Warnings = append(result.Warnings, T("disk.image.detach.busy"))
	}
	return diskGrowOutcome{Result: finishDiskResult(result, started)}
}

func diskDetachDetail(plan []Evidence) string {
	var builder strings.Builder
	for _, evidence := range plan {
		fmt.Fprintf(&builder, "  %s\n    %s\n", evidence.Label, evidence.Value)
	}
	return builder.String()
}

func runDiskDetach(args []string, version string) int {
	options := configuredCommon(time.Minute)
	set := newFlagSet("disk detach")
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	yes := set.Bool("yes", false, T("disk.flag.yes"))
	dryRun := set.Bool("dry-run", false, T("disk.flag.dry_run"))
	set.BoolVar(dryRun, "n", false, T("disk.flag.dry_run"))
	if err := parseReordered(set, args); err != nil {
		return 2
	}
	started := time.Now()
	if !diskImagesSupported() {
		return emit(options, buildReport(version, started, nil, []Result{unsupported(diskProbeDetach, T("disk.image.skip.macos_only"))}, options.redact))
	}
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	images, err := readDiskImages(ctx)
	if err != nil {
		return emit(options, buildReport(version, started, nil, []Result{resultFromError(diskProbeDetach, started, "disk", err)}, options.redact))
	}
	input := diskDetachInput{mounts: set.Args(), dryRun: *dryRun, confirm: diskTerminalConfirm, run: runDiskImageTool}
	if *yes {
		input.confirm = diskAutoConfirm
	}
	outcome := executeDiskDetach(ctx, images, input)
	if outcome.Cancelled {
		fmt.Fprintln(os.Stderr, T("disk.grow.cancelled"))
		return 4
	}
	return emit(options, buildReport(version, started, nil, []Result{outcome.Result}, options.redact))
}
