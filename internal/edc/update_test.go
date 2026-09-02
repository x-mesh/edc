package edc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

func TestFindAsset(t *testing.T) {
	assets := []updateAsset{{Name: "checksums.txt", URL: "u1"}, {Name: "edc_1.0.0_linux_amd64.tar.gz", URL: "u2"}}
	asset, ok := findAsset(assets, "edc_1.0.0_linux_amd64.tar.gz")
	if !ok || asset.URL != "u2" {
		t.Fatalf("asset = %#v, ok = %v", asset, ok)
	}
	if _, ok := findAsset(assets, "edc_1.0.0_darwin_arm64.tar.gz"); ok {
		t.Fatal("missing asset must not match")
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
