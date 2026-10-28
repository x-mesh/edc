package edc

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/x-mesh/fastgzip"
)

const (
	grepMiniLMRevision = "c9745ed1d9f207416be6d2e6f8de32d1f16199bf"
	grepMiniLMName     = "sentence-transformers/all-MiniLM-L6-v2"
	grepRuntimeVersion = "1.23.2"
)

type grepModelAsset struct {
	name, url, sha string
	size           int64
}
type grepRuntimeAsset struct {
	archive                 grepModelAsset
	member, libName, libSHA string
	libSize                 int64
}

var errGrepModelCancelled = errors.New("model download cancelled")

// 전체 다운로드 시간에는 상한을 두지 않는다. 느린 회선에서도 설치가 끝나도록, 데이터가 이 시간 동안 오지 않을 때만 중단한다.
var grepModelIdleTimeout = time.Minute

var grepModelHTTPClient = &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return errors.New("refusing non-HTTPS model redirect")
	}
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	return nil
}}

func grepModelAssets() (grepModelAsset, grepModelAsset, grepRuntimeAsset, error) {
	return grepModelAssetsFor(runtime.GOOS, runtime.GOARCH)
}

func grepModelAssetsFor(goos, goarch string) (grepModelAsset, grepModelAsset, grepRuntimeAsset, error) {
	base := "https://huggingface.co/" + grepMiniLMName + "/resolve/" + grepMiniLMRevision + "/"
	model := grepModelAsset{"model.onnx", base + "onnx/model.onnx", "6fd5d72fe4589f189f8ebc006442dbb529bb7ce38f8082112682524616046452", 90405214}
	vocab := grepModelAsset{"vocab.txt", base + "vocab.txt", "07eced375cec144d27c900241f3e339478dec958f92fddbc551f295c992038a3", 231508}
	release := "https://github.com/microsoft/onnxruntime/releases/download/v" + grepRuntimeVersion + "/"
	var rt grepRuntimeAsset
	switch goos + "/" + goarch {
	case "darwin/amd64", "darwin/arm64":
		name := "onnxruntime-osx-universal2-1.23.2.tgz"
		rt = grepRuntimeAsset{grepModelAsset{name, release + name, "49ae8e3a66ccb18d98ad3fe7f5906b6d7887df8a5edd40f49eb2b14e20885809", 42818339}, "onnxruntime-osx-universal2-1.23.2/lib/libonnxruntime.1.23.2.dylib", "libonnxruntime.dylib", "6fee21a0dbcaa98fe082cb4f7ed07ec5def439df36198f47b61dc205e7d2a1fa", 0}
	case "linux/amd64":
		name := "onnxruntime-linux-x64-1.23.2.tgz"
		rt = grepRuntimeAsset{grepModelAsset{name, release + name, "1fa4dcaef22f6f7d5cd81b28c2800414350c10116f5fdd46a2160082551c5f9b", 8309231}, "onnxruntime-linux-x64-1.23.2/lib/libonnxruntime.so.1.23.2", "libonnxruntime.so", "13ab8084954fa4a47c777880180b90810d6020f021441395712b48a75b74c68b", 0}
	case "linux/arm64":
		name := "onnxruntime-linux-aarch64-1.23.2.tgz"
		rt = grepRuntimeAsset{grepModelAsset{name, release + name, "7c63c73560ed76b1fac6cff8204ffe34fe180e70d6582b5332ec094810241e5c", 7254068}, "onnxruntime-linux-aarch64-1.23.2/lib/libonnxruntime.so.1.23.2", "libonnxruntime.so", "648ffa64fbe027ae27139109410900cf776a030dec2dbbac51053318cc44c286", 0}
	default:
		return model, vocab, rt, fmt.Errorf("%s/%s: %s", goos, goarch, T("grep.model.unsupported"))
	}
	return model, vocab, rt, nil
}

var grepModelManifest = grepModelAssets

func grepModelBase() (string, error) {
	base := grepSemanticCacheRoot
	var err error
	if base == "" {
		base, err = os.UserCacheDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(base, "edc", "models"), nil
}
func grepModelDir() (string, error) {
	base, err := grepModelBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "minilm-l6-v2-"+grepMiniLMRevision+"-ort-"+grepRuntimeVersion+"-"+runtime.GOOS+"-"+runtime.GOARCH), nil
}
func grepModelLock() (func(), error) {
	base, err := grepModelBase()
	if err != nil {
		return nil, err
	}
	return grepFileLock(filepath.Join(base, "model"), syscall.LOCK_EX)
}

func grepModelReadLock() (func(), error) {
	base, err := grepModelBase()
	if err != nil {
		return nil, err
	}
	return grepFileLock(filepath.Join(base, "model"), syscall.LOCK_SH)
}

func grepVerifyFile(path, expected string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || (size > 0 && info.Size() != size) {
		return errors.New("unexpected file size or type")
	}
	if info.Size() > 160<<20 {
		return errors.New("asset exceeds size limit")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return errors.New("SHA-256 mismatch")
	}
	return nil
}

func grepModelCheck(dir string) error {
	model, vocab, rt, err := grepModelManifest()
	if err != nil {
		return err
	}
	return grepCheckModelAssets(dir, model, vocab, rt)
}

func grepCheckModelAssets(dir string, model, vocab grepModelAsset, rt grepRuntimeAsset) error {
	for _, asset := range []grepModelAsset{model, vocab, {rt.libName, "", rt.libSHA, rt.libSize}} {
		if err := grepVerifyFile(filepath.Join(dir, asset.name), asset.sha, asset.size); err != nil {
			return fmt.Errorf("%s: %w", asset.name, err)
		}
	}
	return nil
}

func grepDownloadAsset(ctx context.Context, client *http.Client, asset grepModelAsset, dest string, progress io.Writer) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	idle := time.AfterFunc(grepModelIdleTimeout, func() {
		cancel(fmt.Errorf("%s: no data for %s", asset.name, grepModelIdleTimeout))
	})
	defer idle.Stop()
	err := grepDownloadAssetBody(ctx, client, asset, dest, progress, func() { idle.Reset(grepModelIdleTimeout) })
	if cause := context.Cause(ctx); err != nil && cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return err
}

func grepDownloadAssetBody(ctx context.Context, client *http.Client, asset grepModelAsset, dest string, progress io.Writer, received func()) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "edc-semantic")
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", asset.name, response.StatusCode)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		f.Close()
		if !success {
			os.Remove(dest)
		}
	}()
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	reader := io.LimitReader(response.Body, asset.size+1)
	var total int64
	last := time.Now()
	terminal := false
	if output, ok := progress.(*os.File); ok {
		terminal = isTerminal(output)
	}
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			received()
			if _, err := f.Write(buffer[:n]); err != nil {
				return err
			}
			hash.Write(buffer[:n])
			total += int64(n)
			if terminal && time.Since(last) >= time.Second {
				fmt.Fprintf(progress, "\r%s: %d/%d MiB", asset.name, total>>20, (asset.size+(1<<20)-1)>>20)
				last = time.Now()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if terminal {
		fmt.Fprint(progress, "\r")
	}
	fmt.Fprintf(progress, "%s: %d/%d MiB\n", asset.name, (total+(1<<20)-1)>>20, (asset.size+(1<<20)-1)>>20)
	if total != asset.size || hex.EncodeToString(hash.Sum(nil)) != asset.sha {
		return fmt.Errorf("%s: size or SHA-256 mismatch", asset.name)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	success = true
	return nil
}

func grepExtractRuntime(archive, destination string, rt grepRuntimeAsset) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := fastgzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := false
	notices := map[string]bool{"LICENSE": false, "ThirdPartyNotices.txt": false}
	prefix, _, ok := strings.Cut(rt.member, "/lib/")
	if !ok {
		return errors.New("invalid runtime member path")
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if label, ok := strings.CutPrefix(name, prefix+"/"); ok {
			if present, wanted := notices[label]; wanted {
				if present || h.Typeflag != tar.TypeReg || h.Size <= 0 || h.Size > 8<<20 {
					return errors.New("invalid runtime license file")
				}
				data, err := io.ReadAll(io.LimitReader(tr, h.Size+1))
				if err != nil || int64(len(data)) != h.Size {
					return errors.New("cannot read runtime license file")
				}
				if err := os.WriteFile(filepath.Join(filepath.Dir(destination), "ONNX-"+label), data, 0600); err != nil {
					return err
				}
				notices[label] = true
				continue
			}
		}
		if name != rt.member {
			continue
		}
		if found || h.Typeflag != tar.TypeReg || h.Size <= 0 || h.Size > 160<<20 {
			return errors.New("invalid runtime archive")
		}
		out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		hash := sha256.New()
		n, err := io.Copy(io.MultiWriter(out, hash), tr)
		if err == nil {
			err = out.Sync()
		}
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if n != h.Size || hex.EncodeToString(hash.Sum(nil)) != rt.libSHA {
			return errors.New("runtime SHA-256 mismatch")
		}
		found = true
	}
	if !found {
		return errors.New("runtime library missing from archive")
	}
	for _, present := range notices {
		if !present {
			return errors.New("runtime license file missing from archive")
		}
	}
	return nil
}

func grepInstallModel(ctx context.Context, client *http.Client, dir string, model, vocab grepModelAsset, rt grepRuntimeAsset, progress io.Writer) error {
	base := filepath.Dir(dir)
	if err := os.MkdirAll(base, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(base, ".minilm-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for _, asset := range []grepModelAsset{model, vocab, rt.archive} {
		if err := grepDownloadAsset(ctx, client, asset, filepath.Join(stage, asset.name), progress); err != nil {
			return err
		}
	}
	archive := filepath.Join(stage, rt.archive.name)
	if err := grepExtractRuntime(archive, filepath.Join(stage, rt.libName), rt); err != nil {
		return err
	}
	if err := os.Remove(archive); err != nil {
		return err
	}
	if err := grepCheckModelAssets(stage, model, vocab, rt); err != nil {
		return err
	}
	backup := ""
	if _, err := os.Lstat(dir); err == nil {
		temp, err := os.MkdirTemp(base, ".minilm-old-")
		if err != nil {
			return err
		}
		if err := os.Remove(temp); err != nil {
			return err
		}
		if err := os.Rename(dir, temp); err != nil {
			return err
		}
		backup = temp
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(stage, dir); err != nil {
		if backup != "" {
			if restore := os.Rename(backup, dir); restore != nil {
				return fmt.Errorf("%v; restore %s: %w", err, backup, restore)
			}
		}
		return err
	}
	if backup != "" {
		if err := os.RemoveAll(backup); err != nil {
			return err
		}
	}
	return nil
}

func grepEnsureModel(input io.Reader, terminal, yes bool, output io.Writer) error {
	dir, err := grepModelDir()
	if err != nil {
		return err
	}
	if err := grepModelCheck(dir); err == nil {
		return nil
	}
	model, vocab, rt, err := grepModelManifest()
	if err != nil {
		return err
	}
	fmt.Fprintln(output, T("grep.model.preview", grepMiniLMName, (model.size+vocab.size+rt.archive.size+(1<<20)-1)>>20, dir))
	if !yes {
		if !terminal {
			return errors.New(T("grep.model.noninteractive"))
		}
		fmt.Fprint(output, T("grep.model.confirm"))
		line, readErr := bufio.NewReader(input).ReadString('\n')
		if readErr != nil && len(line) == 0 {
			return errGrepModelCancelled
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			return errGrepModelCancelled
		}
	}
	unlock, err := grepModelLock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := grepModelCheck(dir); err == nil {
		return nil
	}
	return grepInstallModel(context.Background(), grepModelHTTPClient, dir, model, vocab, rt, output)
}

var grepSemanticEnsure = grepEnsureModel
