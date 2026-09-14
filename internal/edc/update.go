package edc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"
)

const (
	updateRepo = "x-mesh/edc"
	// updateReleasesURL은 github.com의 release 경로다. api.github.com의 익명 요청은 IP마다 시간당 60회로 제한되어,
	// 같은 IP의 다른 도구가 한도를 쓰면 edc update가 403으로 멈췄다. latest redirect와 download 경로는 이 한도를 쓰지 않는다.
	updateReleasesURL = "https://github.com/" + updateRepo + "/releases"
	// updateChecksumName은 release가 함께 올리는 SHA-256 목록이다.
	updateChecksumName = "checksums.txt"
	// maxUpdateAsset은 내려받는 asset의 상한이다. 잘못된 asset으로 memory를 다 쓰지 않게 막는다.
	maxUpdateAsset = 64 << 20
	// updateLabelWidth는 번역된 label이 섞여도 값 열이 어긋나지 않게 맞추는 표시 폭이다.
	updateLabelWidth = 10
)

func runUpdate(args []string, version string) int {
	set := flag.NewFlagSet("update", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	check := set.Bool("check", false, T("command.update.option.check"))
	yes := set.Bool("yes", false, T("command.update.option.yes"))
	timeout := set.Duration("timeout", configuredDurationFallback(activeConfig.Defaults.Update.Timeout, activeConfig.Defaults.Common.Timeout, 60*time.Second), T("command.update.option.timeout"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc update [--check] [--yes] [--timeout 60s]"))
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	tag, err := latestReleaseTag(ctx, updateReleasesURL, version)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	latest := strings.TrimPrefix(tag, "v")
	current := strings.TrimPrefix(version, "v")
	if latest == current {
		fmt.Println(T("cli.update.already_latest", current))
		return 0
	}

	fmt.Println(T("cli.update.versions", current, latest))
	if *check {
		fmt.Println(updateReleasesURL + "/tag/" + tag)
		return 0
	}

	assetName := updateAssetName(latest)
	downloadURL := updateReleasesURL + "/download/" + tag + "/"
	// checksums.txt는 release의 모든 archive를 한 줄씩 담는다. 확인을 묻기 전에 받아 이 platform의 asset이 있는지 본다.
	sumList, err := downloadAsset(ctx, downloadURL+updateChecksumName, version)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if checksumFor(sumList, assetName) == "" {
		fmt.Fprintln(os.Stderr, T("cli.update.asset_missing", assetName))
		return 2
	}

	target, err := updateTargetPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := checkWritable(filepath.Dir(target)); err != nil {
		fmt.Fprintln(os.Stderr, T("cli.update.not_writable", filepath.Dir(target), err))
		return 3
	}

	if !*yes && !confirmUpdate(os.Stdin, os.Stdout, updateDetail(current, latest, assetName, target)) {
		fmt.Fprintln(os.Stderr, T("cli.update.cancelled"))
		return 4
	}

	archive, err := downloadAsset(ctx, downloadURL+assetName, version)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := verifyChecksum(archive, assetName, sumList); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	binary, err := extractBinary(archive)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := replaceBinary(target, binary); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	fmt.Println(T("cli.update.replaced", latest, target))
	return 0
}

// updateAssetName은 release workflow와 install.sh가 쓰는 이름 규칙을 따른다.
func updateAssetName(version string) string {
	return fmt.Sprintf("edc_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
}

// latestReleaseTag는 releases/latest가 돌려주는 redirect의 Location에서 tag를 읽는다.
// redirect를 따라가지 않으므로 release 페이지 본문은 받지 않는다.
func latestReleaseTag(ctx context.Context, releasesURL, version string) (string, error) {
	latestURL := releasesURL + "/latest"
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, latestURL, nil)
	if err != nil {
		return "", fmt.Errorf("%s: %w", T("cli.update.request_failed"), err)
	}
	request.Header.Set("User-Agent", "edc/"+version)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("%s: %w", T("cli.update.fetch_failed", latestURL), err)
	}
	response.Body.Close()
	location, err := response.Location()
	if err != nil {
		return "", errors.New(T("cli.update.http_status", latestURL, response.StatusCode))
	}
	_, tag, found := strings.Cut(location.Path, "/releases/tag/")
	if !found || tag == "" || strings.Contains(tag, "/") {
		return "", errors.New(T("cli.update.no_tag"))
	}
	return tag, nil
}

func downloadAsset(ctx context.Context, url, version string) ([]byte, error) {
	return fetch(ctx, url, version)
}

func fetch(ctx context.Context, url, version string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", T("cli.update.request_failed"), err)
	}
	request.Header.Set("User-Agent", "edc/"+version)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", T("cli.update.fetch_failed", url), err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, errors.New(T("cli.update.http_status", url, response.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxUpdateAsset+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", T("cli.update.response_read_failed"), err)
	}
	if int64(len(body)) > maxUpdateAsset {
		return nil, errors.New(T("cli.update.asset_too_large", int64(maxUpdateAsset)))
	}
	return body, nil
}

// checksumFor는 `<sha256>  <name>` 형식의 목록에서 name의 digest를 찾는다. 없으면 빈 문자열이다.
func checksumFor(list []byte, name string) string {
	for _, line := range strings.Split(string(list), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0])
		}
	}
	return ""
}

// verifyChecksum은 목록에서 name의 줄을 찾아 data의 SHA-256과 대조한다.
func verifyChecksum(data []byte, name string, list []byte) error {
	want := checksumFor(list, name)
	if want == "" {
		return errors.New(T("cli.update.checksum_missing", name, updateChecksumName))
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if got != want {
		return errors.New(T("cli.update.checksum_mismatch", want, got))
	}
	return nil
}

// extractBinary는 tar.gz에서 edc 실행 파일 하나를 꺼낸다.
func extractBinary(archive []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", T("cli.update.asset_open_failed"), err)
	}
	defer reader.Close()

	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", T("cli.update.asset_read_failed"), err)
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != "edc" {
			continue
		}
		binary, err := io.ReadAll(io.LimitReader(tarReader, maxUpdateAsset))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", T("cli.update.binary_read_failed"), err)
		}
		if len(binary) == 0 {
			return nil, errors.New(T("cli.update.binary_empty"))
		}
		return binary, nil
	}
	return nil, errors.New(T("cli.update.binary_absent"))
}

func updateTargetPath() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("%s: %w", T("cli.update.executable_path_failed"), err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", T("cli.update.executable_resolve_failed"), err)
	}
	return resolved, nil
}

// checkWritable은 대상 디렉터리에 임시 파일을 만들어 권한을 확인한다.
// 내려받기 전에 확인해야 실패했을 때 network 사용이 헛되지 않는다.
func checkWritable(dir string) error {
	file, err := os.CreateTemp(dir, ".edc-write-check-*")
	if err != nil {
		return err
	}
	name := file.Name()
	file.Close()
	return os.Remove(name)
}

// replaceBinary는 같은 디렉터리에 새 파일을 쓰고 rename으로 바꾼다.
// rename은 같은 file system 안에서 원자적이라 중간 상태가 남지 않는다.
func replaceBinary(target string, data []byte) error {
	dir := filepath.Dir(target)
	file, err := os.CreateTemp(dir, ".edc-update-*")
	if err != nil {
		return fmt.Errorf("%s: %w", T("cli.update.temp_create_failed"), err)
	}
	temp := file.Name()

	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(temp)
		return fmt.Errorf("%s: %w", T("cli.update.temp_write_failed"), err)
	}
	if err := file.Close(); err != nil {
		os.Remove(temp)
		return fmt.Errorf("%s: %w", T("cli.update.temp_close_failed"), err)
	}
	if err := os.Chmod(temp, 0o755); err != nil {
		os.Remove(temp)
		return fmt.Errorf("%s: %w", T("cli.update.chmod_failed"), err)
	}
	if err := os.Rename(temp, target); err != nil {
		os.Remove(temp)
		return fmt.Errorf("%s: %w", T("cli.update.rename_failed", target), err)
	}
	return nil
}

func updateDetail(current, latest, assetName, target string) string {
	rows := [][2]string{
		{T("cli.update.label.current"), current},
		{T("cli.update.label.latest"), latest},
		{T("cli.update.label.asset"), assetName},
		{T("cli.update.label.target"), target},
	}
	var builder strings.Builder
	for _, row := range rows {
		builder.WriteString(liveCell(row[0], updateLabelWidth) + row[1] + "\n")
	}
	return builder.String()
}

// confirmUpdate는 terminal에서는 확인 화면을, 그 외에는 y/N 입력을 쓴다.
func confirmUpdate(input *os.File, output *os.File, detail string) bool {
	if term.IsTerminal(int(input.Fd())) {
		answer, err := runConfirmModel(input, output, newDetailedConfirmModel(detail, T("cli.update.confirm"), false))
		return err == nil && answer
	}
	fmt.Fprint(output, detail)
	return confirm(input, output)
}
