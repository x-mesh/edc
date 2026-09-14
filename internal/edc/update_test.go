package edc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUpdateAssetNameMatchesReleaseNaming(t *testing.T) {
	name := updateAssetName("1.2.3")
	want := fmt.Sprintf("edc_1.2.3_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	if name != want {
		t.Fatalf("asset name = %q, want %q", name, want)
	}
}

// latestReleaseTag는 redirect를 따라가지 않는다. 따라가면 tag 페이지 경로가 403을 돌려 test가 실패한다.
func TestLatestReleaseTagReadsRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/tagged/latest":
			http.Redirect(writer, request, "/x-mesh/edc/releases/tag/v1.2.3", http.StatusFound)
		case "/untagged/latest":
			http.Redirect(writer, request, "/x-mesh/edc/releases", http.StatusFound)
		default:
			writer.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()
	ctx := context.Background()
	if tag, err := latestReleaseTag(ctx, server.URL+"/tagged", "test"); err != nil || tag != "v1.2.3" {
		t.Fatalf("tag = %q, err = %v", tag, err)
	}
	if _, err := latestReleaseTag(ctx, server.URL+"/untagged", "test"); err == nil || err.Error() != T("cli.update.no_tag") {
		t.Fatalf("redirect without a tag returned %v", err)
	}
	if _, err := latestReleaseTag(ctx, server.URL+"/blocked", "test"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a response without redirect returned %v", err)
	}
}

func TestChecksumForFindsListedAsset(t *testing.T) {
	list := []byte("ABCD  edc_1.0.0_linux_amd64.tar.gz\n")
	if got := checksumFor(list, "edc_1.0.0_linux_amd64.tar.gz"); got != "abcd" {
		t.Fatalf("listed digest = %q", got)
	}
	if got := checksumFor(list, "edc_1.0.0_darwin_arm64.tar.gz"); got != "" {
		t.Fatalf("unlisted digest = %q", got)
	}
}

func TestVerifyChecksum(t *testing.T) {
	data := []byte("binary payload")
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	list := []byte("0000  other.tar.gz\n" + digest + "  edc_1.0.0_linux_amd64.tar.gz\n")

	if err := verifyChecksum(data, "edc_1.0.0_linux_amd64.tar.gz", list); err != nil {
		t.Fatalf("matching checksum returned %v", err)
	}
	if err := verifyChecksum([]byte("tampered"), "edc_1.0.0_linux_amd64.tar.gz", list); err == nil {
		t.Fatal("changed payload must fail")
	}
	if err := verifyChecksum(data, "edc_1.0.0_darwin_arm64.tar.gz", list); err == nil {
		t.Fatal("missing entry must fail")
	}
}

func TestVerifyChecksumAcceptsBinaryMarker(t *testing.T) {
	data := []byte("payload")
	sum := sha256.Sum256(data)
	list := []byte(hex.EncodeToString(sum[:]) + " *edc_1.0.0_linux_amd64.tar.gz\n")
	if err := verifyChecksum(data, "edc_1.0.0_linux_amd64.tar.gz", list); err != nil {
		t.Fatalf("sha256sum binary marker returned %v", err)
	}
}

func TestExtractBinary(t *testing.T) {
	archive := buildArchive(t, map[string]string{"LICENSE": "text", "edc": "executable"})
	binary, err := extractBinary(archive)
	if err != nil {
		t.Fatalf("extract returned %v", err)
	}
	if string(binary) != "executable" {
		t.Fatalf("binary = %q", binary)
	}
}

func TestExtractBinaryWithoutEdcFails(t *testing.T) {
	archive := buildArchive(t, map[string]string{"README.md": "text"})
	if _, err := extractBinary(archive); err == nil {
		t.Fatal("archive without edc must fail")
	}
}

func TestExtractBinaryRejectsBrokenArchive(t *testing.T) {
	if _, err := extractBinary([]byte("not a gzip stream")); err == nil {
		t.Fatal("broken archive must fail")
	}
}

func TestReplaceBinaryKeepsPathAndMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "edc")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceBinary(target, []byte("new")); err != nil {
		t.Fatalf("replace returned %v", err)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "new" {
		t.Fatalf("content = %q", content)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".edc-update-") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestCheckWritable(t *testing.T) {
	dir := t.TempDir()
	if err := checkWritable(dir); err != nil {
		t.Fatalf("writable directory returned %v", err)
	}
	if err := checkWritable(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing directory must fail")
	}
}

func TestUpdateDetailListsVersionsAndTarget(t *testing.T) {
	detail := updateDetail("0.1.0", "0.2.0", "edc_0.2.0_linux_amd64.tar.gz", "/usr/local/bin/edc")
	for _, want := range []string{"0.1.0", "0.2.0", "edc_0.2.0_linux_amd64.tar.gz", "/usr/local/bin/edc"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail %q missing %q", detail, want)
		}
	}
}

func buildArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	zip := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(zip)
	for name, body := range files {
		header := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
