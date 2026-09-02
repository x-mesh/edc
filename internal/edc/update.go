package edc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	updateRepo      = "x-mesh/edc"
	updateLatestURL = "https://api.github.com/repos/" + updateRepo + "/releases/latest"
	// updateChecksumName은 release가 함께 올리는 SHA-256 목록이다.
	updateChecksumName = "checksums.txt"
	// maxUpdateAsset은 내려받는 asset의 상한이다. 잘못된 asset으로 memory를 다 쓰지 않게 막는다.
	maxUpdateAsset = 64 << 20
)

type updateRelease struct {
	TagName string        `json:"tag_name"`
	HTMLURL string        `json:"html_url"`
	Assets  []updateAsset `json:"assets"`
}

type updateAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

func runUpdate(args []string, version string) int {
	set := flag.NewFlagSet("update", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	check := set.Bool("check", false, "새 버전만 확인하고 설치하지 않습니다")
	yes := set.Bool("yes", false, "interactive 확인 생략")
	timeout := set.Duration("timeout", 60*time.Second, "network timeout")
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "사용법: edc update [--check] [--yes] [--timeout 60s]")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	release, err := latestRelease(ctx, version)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	latest := strings.TrimPrefix(release.TagName, "v")
	current := strings.TrimPrefix(version, "v")
	if latest == "" {
		fmt.Fprintln(os.Stderr, "release에 tag가 없습니다")
		return 2
	}
	if latest == current {
		fmt.Printf("edc %s는 이미 최신입니다\n", current)
		return 0
	}

	fmt.Printf("현재 %s  ·  최신 %s\n", current, latest)
	if *check {
		fmt.Println(release.HTMLURL)
		return 0
	}

	assetName := updateAssetName(latest)
	asset, ok := findAsset(release.Assets, assetName)
	if !ok {
		fmt.Fprintf(os.Stderr, "이 platform의 asset이 release에 없습니다: %s\n", assetName)
		return 2
	}
	sums, ok := findAsset(release.Assets, updateChecksumName)
	if !ok {
		fmt.Fprintf(os.Stderr, "release에 %s가 없어 checksum을 확인할 수 없습니다\n", updateChecksumName)
		return 2
	}

	target, err := updateTargetPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := checkWritable(filepath.Dir(target)); err != nil {
		fmt.Fprintf(os.Stderr, "%s에 쓸 권한이 없습니다: %v\n", filepath.Dir(target), err)
		return 3
	}

	if !*yes && !confirmUpdate(os.Stdin, os.Stdout, updateDetail(current, latest, assetName, target)) {
		fmt.Fprintln(os.Stderr, "update를 취소했습니다")
		return 4
	}

	archive, err := downloadAsset(ctx, asset.URL, version)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	sumList, err := downloadAsset(ctx, sums.URL, version)
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

	fmt.Printf("edc %s로 업데이트했습니다  ·  %s\n", latest, target)
	return 0
}

// updateAssetName은 release workflow와 install.sh가 쓰는 이름 규칙을 따른다.
func updateAssetName(version string) string {
	return fmt.Sprintf("edc_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
}

func findAsset(assets []updateAsset, name string) (updateAsset, bool) {
	for _, asset := range assets {
		if asset.Name == name {
			return asset, true
		}
	}
	return updateAsset{}, false
}

func latestRelease(ctx context.Context, version string) (updateRelease, error) {
	body, err := fetch(ctx, updateLatestURL, version, map[string]string{"Accept": "application/vnd.github+json"})
	if err != nil {
		return updateRelease{}, err
	}
	var release updateRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return updateRelease{}, fmt.Errorf("release 정보를 읽지 못했습니다: %w", err)
	}
	return release, nil
}

func downloadAsset(ctx context.Context, url, version string) ([]byte, error) {
	return fetch(ctx, url, version, nil)
}

func fetch(ctx context.Context, url, version string, headers map[string]string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("요청을 만들지 못했습니다: %w", err)
	}
	request.Header.Set("User-Agent", "edc/"+version)
	for key, value := range headers {
		request.Header.Set(key, value)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s 요청이 실패했습니다: %w", url, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s가 HTTP %d를 돌려주었습니다", url, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxUpdateAsset+1))
	if err != nil {
		return nil, fmt.Errorf("응답을 읽지 못했습니다: %w", err)
	}
	if int64(len(body)) > maxUpdateAsset {
		return nil, fmt.Errorf("asset이 상한 %d bytes를 넘습니다", int64(maxUpdateAsset))
	}
	return body, nil
}

// verifyChecksum은 `<sha256>  <name>` 형식의 목록에서 name의 줄을 찾아 대조한다.
func verifyChecksum(data []byte, name string, list []byte) error {
	want := ""
	for _, line := range strings.Split(string(list), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			want = strings.ToLower(fields[0])
			break
		}
	}
	if want == "" {
		return fmt.Errorf("%s의 checksum이 %s에 없습니다", name, updateChecksumName)
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if got != want {
		return fmt.Errorf("checksum이 맞지 않습니다: 기대 %s, 실제 %s", want, got)
	}
	return nil
}

// extractBinary는 tar.gz에서 edc 실행 파일 하나를 꺼낸다.
func extractBinary(archive []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("asset을 열지 못했습니다: %w", err)
	}
	defer reader.Close()

	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("asset을 읽지 못했습니다: %w", err)
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != "edc" {
			continue
		}
		binary, err := io.ReadAll(io.LimitReader(tarReader, maxUpdateAsset))
		if err != nil {
			return nil, fmt.Errorf("실행 파일을 읽지 못했습니다: %w", err)
		}
		if len(binary) == 0 {
			return nil, fmt.Errorf("asset의 edc 실행 파일이 비어 있습니다")
		}
		return binary, nil
	}
	return nil, fmt.Errorf("asset에 edc 실행 파일이 없습니다")
}

func updateTargetPath() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("실행 파일 경로를 찾지 못했습니다: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("실행 파일 경로를 확인하지 못했습니다: %w", err)
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
		return fmt.Errorf("임시 파일을 만들지 못했습니다: %w", err)
	}
	temp := file.Name()

	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(temp)
		return fmt.Errorf("임시 파일에 쓰지 못했습니다: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(temp)
		return fmt.Errorf("임시 파일을 닫지 못했습니다: %w", err)
	}
	if err := os.Chmod(temp, 0o755); err != nil {
		os.Remove(temp)
		return fmt.Errorf("실행 권한을 주지 못했습니다: %w", err)
	}
	if err := os.Rename(temp, target); err != nil {
		os.Remove(temp)
		return fmt.Errorf("%s를 바꾸지 못했습니다: %w", target, err)
	}
	return nil
}

func updateDetail(current, latest, assetName, target string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "현재      %s\n", current)
	fmt.Fprintf(&builder, "최신      %s\n", latest)
	fmt.Fprintf(&builder, "asset     %s\n", assetName)
	fmt.Fprintf(&builder, "설치 경로 %s\n", target)
	return builder.String()
}

// confirmUpdate는 terminal에서는 확인 화면을, 그 외에는 y/N 입력을 쓴다.
func confirmUpdate(input *os.File, output *os.File, detail string) bool {
	if term.IsTerminal(int(input.Fd())) {
		answer, err := runConfirmModel(input, output, newDetailedConfirmModel(detail, "업데이트할까요?", false))
		return err == nil && answer
	}
	fmt.Fprint(output, detail)
	return confirm(input, output)
}
