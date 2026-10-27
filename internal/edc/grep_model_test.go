package edc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGrepModelDownloadConfirmationIntegrityAndCleanup(t *testing.T) {
	oldRoot, oldManifest, oldClient := grepSemanticCacheRoot, grepModelManifest, grepModelHTTPClient
	defer func() {
		grepSemanticCacheRoot = oldRoot
		grepModelManifest = oldManifest
		grepModelHTTPClient = oldClient
	}()
	grepSemanticCacheRoot = filepath.Join(t.TempDir(), "cache")
	modelBytes := []byte("model data")
	vocabBytes := []byte("vocab data")
	libBytes := []byte("runtime library")
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gz)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "release/lib/runtime", Mode: 0600, Size: int64(len(libBytes)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(libBytes); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"release/LICENSE", "release/ThirdPartyNotices.txt"} {
		data := []byte("notice")
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	blobs := map[string][]byte{"/model": modelBytes, "/vocab": vocabBytes, "/runtime": archive.Bytes()}
	var calls atomic.Int32
	var broken atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if broken.Load() && r.URL.Path == "/model" {
			fmt.Fprint(w, "wrong")
			return
		}
		b, ok := blobs[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	defer server.Close()
	sha := func(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
	model := grepModelAsset{"model.onnx", server.URL + "/model", sha(modelBytes), int64(len(modelBytes))}
	vocab := grepModelAsset{"vocab.txt", server.URL + "/vocab", sha(vocabBytes), int64(len(vocabBytes))}
	rt := grepRuntimeAsset{archive: grepModelAsset{"runtime.tgz", server.URL + "/runtime", sha(archive.Bytes()), int64(archive.Len())}, member: "release/lib/runtime", libName: "runtime.so", libSHA: sha(libBytes), libSize: int64(len(libBytes))}
	grepModelManifest = func() (grepModelAsset, grepModelAsset, grepRuntimeAsset, error) { return model, vocab, rt, nil }
	grepModelHTTPClient = server.Client()
	var output bytes.Buffer
	if err := grepEnsureModel(strings.NewReader(""), false, false, &output); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("noninteractive error %v", err)
	}
	if _, err := os.Stat(grepSemanticCacheRoot); !os.IsNotExist(err) || calls.Load() != 0 {
		t.Fatalf("preflight wrote or fetched: %v calls %d", err, calls.Load())
	}
	if err := grepEnsureModel(strings.NewReader("n\n"), true, false, &output); !errors.Is(err, errGrepModelCancelled) || calls.Load() != 0 {
		t.Fatalf("cancel error %v calls %d", err, calls.Load())
	}
	if err := grepEnsureModel(strings.NewReader(""), false, true, &output); err != nil {
		t.Fatal(err)
	}
	dir, err := grepModelDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := grepModelCheck(dir); err != nil {
		t.Fatal(err)
	}
	if err := grepModelQuickCheck(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, grepModelStampName)); err != nil {
		t.Fatalf("verified stamp missing: %v", err)
	}
	if err := grepModelQuickCheck(dir); err != nil {
		t.Fatalf("stamped check: %v", err)
	}
	for _, name := range []string{model.name, vocab.name, rt.libName} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("%s: %v %v", name, info, err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("download calls %d", calls.Load())
	}
	if err := grepEnsureModel(strings.NewReader(""), false, false, &output); err != nil || calls.Load() != 3 {
		t.Fatalf("reuse: %v calls %d", err, calls.Load())
	}
	damaged := filepath.Join(dir, model.name)
	if err := os.WriteFile(damaged, []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := grepModelQuickCheck(dir); err == nil {
		t.Fatal("stamp hid a changed model")
	}
	broken.Store(true)
	if err := grepEnsureModel(strings.NewReader(""), false, true, &output); err == nil {
		t.Fatal("corrupt download accepted")
	}
	preserved, err := os.ReadFile(damaged)
	if err != nil || string(preserved) != "damaged" {
		t.Fatalf("previous cache changed: %q %v", preserved, err)
	}
	broken.Store(false)
	if err := grepEnsureModel(strings.NewReader(""), false, true, &output); err != nil {
		t.Fatal(err)
	}
	if err := grepModelCheck(dir); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	canonical, err := grepSemanticRoot([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	indexPath, err := grepSemanticPath(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if err := grepSemanticSave(indexPath, &grepSemanticIndex{Version: 1, Root: canonical, Model: grepSemanticModel, Revision: "test", Vectors: map[string][]float32{}}); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if code := grepSemanticLifecycleWithInput([]string{"--model-clear"}, strings.NewReader(""), false, &output, io.Discard); code != 0 {
		t.Fatalf("clear exit %d", code)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("model remains: %v", err)
	}
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("index removed: %v", err)
	}
	output.Reset()
	if err := grepEnsureModel(strings.NewReader("y\n"), true, false, &output); err != nil {
		t.Fatalf("interactive confirmation: %v", err)
	}
	if !strings.Contains(output.String(), "[y/N]") {
		t.Fatalf("confirmation missing: %s", output.String())
	}
}

func TestGrepExtractRuntimeRejectsSymlinks(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	w := tar.NewWriter(gz)
	if err := w.WriteHeader(&tar.Header{Name: "release/lib/runtime", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.tgz")
	if err := os.WriteFile(path, archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := grepExtractRuntime(path, filepath.Join(dir, "runtime.so"), grepRuntimeAsset{member: "release/lib/runtime"}); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestGrepModelAssetsCoverReleasePlatforms(t *testing.T) {
	for _, platform := range []struct{ os, arch, archive string }{
		{"darwin", "amd64", "onnxruntime-osx-universal2-1.23.2.tgz"},
		{"darwin", "arm64", "onnxruntime-osx-universal2-1.23.2.tgz"},
		{"linux", "amd64", "onnxruntime-linux-x64-1.23.2.tgz"},
		{"linux", "arm64", "onnxruntime-linux-aarch64-1.23.2.tgz"},
	} {
		model, vocab, rt, err := grepModelAssetsFor(platform.os, platform.arch)
		if err != nil || rt.archive.name != platform.archive || len(model.sha) != 64 || len(vocab.sha) != 64 || len(rt.archive.sha) != 64 || len(rt.libSHA) != 64 {
			t.Errorf("%s/%s: %+v %v", platform.os, platform.arch, rt, err)
		}
	}
	if _, _, _, err := grepModelAssetsFor("linux", "riscv64"); err == nil {
		t.Fatal("unsupported platform accepted")
	}
}

func TestGrepOfficialRuntimeArchives(t *testing.T) {
	tested := 0
	for _, tc := range []struct{ env, os, arch string }{
		{"EDC_TEST_ONNX_ARCHIVE", "darwin", "arm64"},
		{"EDC_TEST_ONNX_LINUX_AMD64", "linux", "amd64"},
		{"EDC_TEST_ONNX_LINUX_ARM64", "linux", "arm64"},
	} {
		path := os.Getenv(tc.env)
		if path == "" {
			continue
		}
		tested++
		_, _, rt, err := grepModelAssetsFor(tc.os, tc.arch)
		if err != nil {
			t.Fatal(err)
		}
		if err := grepVerifyFile(path, rt.archive.sha, rt.archive.size); err != nil {
			t.Fatalf("%s archive: %v", tc.env, err)
		}
		dest := filepath.Join(t.TempDir(), rt.libName)
		if err := grepExtractRuntime(path, dest, rt); err != nil {
			t.Fatalf("%s extraction: %v", tc.env, err)
		}
		if err := grepVerifyFile(dest, rt.libSHA, 0); err != nil {
			t.Fatalf("%s library: %v", tc.env, err)
		}
	}
	if tested == 0 {
		t.Skip("set EDC_TEST_ONNX_ARCHIVE or EDC_TEST_ONNX_LINUX_* archive paths")
	}
}

func TestGrepModelDownloadStopsOnlyWhenIdle(t *testing.T) {
	old := grepModelIdleTimeout
	defer func() { grepModelIdleTimeout = old }()
	grepModelIdleTimeout = 200 * time.Millisecond
	payload := []byte("slow but steady model bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		if r.URL.Path == "/stall" {
			w.Write(payload[:1])
			flusher.Flush()
			<-r.Context().Done()
			return
		}
		for _, b := range payload {
			w.Write([]byte{b})
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer server.Close()
	sum := sha256.Sum256(payload)
	asset := grepModelAsset{"model.onnx", server.URL + "/slow", hex.EncodeToString(sum[:]), int64(len(payload))}
	dir := t.TempDir()
	if err := grepDownloadAsset(context.Background(), server.Client(), asset, filepath.Join(dir, "slow"), io.Discard); err != nil {
		t.Fatalf("steady download longer than the idle timeout failed: %v", err)
	}
	asset.url = server.URL + "/stall"
	err := grepDownloadAsset(context.Background(), server.Client(), asset, filepath.Join(dir, "stall"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no data") {
		t.Fatalf("stalled download error %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stall")); !os.IsNotExist(err) {
		t.Fatalf("partial download kept: %v", err)
	}
}
