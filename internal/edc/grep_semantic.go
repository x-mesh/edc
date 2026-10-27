package edc

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const grepSemanticModel = grepMiniLMName

type grepSemanticIndex struct {
	Version  int
	Root     string
	Model    string
	Revision string
	Vectors  map[string][]float32
}

type grepSemanticEncoded struct {
	Revision string      `json:"revision"`
	Vectors  [][]float32 `json:"vectors"`
}

var grepSemanticEncode = grepSemanticNative

func grepSemanticNative(texts []string, progress io.Writer) (grepSemanticEncoded, error) {
	dir, err := grepModelDir()
	if err != nil {
		return grepSemanticEncoded{}, err
	}
	if _, err := os.Stat(dir); err != nil {
		return grepSemanticEncoded{}, fmt.Errorf("%s: %w", T("grep.model.missing"), err)
	}
	unlock, err := grepModelLock()
	if err != nil {
		return grepSemanticEncoded{}, err
	}
	defer unlock()
	if err := grepModelCheck(dir); err != nil {
		return grepSemanticEncoded{}, fmt.Errorf("%s: %w", T("grep.model.missing"), err)
	}
	_, _, rt, err := grepModelManifest()
	if err != nil {
		return grepSemanticEncoded{}, err
	}
	tokenizer, err := newGrepTokenizer(filepath.Join(dir, "vocab.txt"))
	if err != nil {
		return grepSemanticEncoded{}, err
	}
	engine, err := newGrepONNX(filepath.Join(dir, rt.libName), filepath.Join(dir, "model.onnx"))
	if err != nil {
		return grepSemanticEncoded{}, fmt.Errorf("%s: %w", T("grep.semantic.backend_error"), err)
	}
	defer engine.close()
	type tokenizedText struct {
		index int
		ids   []int64
	}
	items := make([]tokenizedText, len(texts))
	for i, text := range texts {
		items[i] = tokenizedText{index: i, ids: tokenizer.encode(text)}
	}
	sort.SliceStable(items, func(i, j int) bool { return len(items[i].ids) > len(items[j].ids) })
	vectors := make([][]float32, len(texts))
	lastProgress := time.Now()
	for start := 0; start < len(items); start += 32 {
		end := min(len(items), start+32)
		batch := make([][]int64, 0, end-start)
		for _, item := range items[start:end] {
			batch = append(batch, item.ids)
		}
		encoded, err := engine.encode(batch)
		if err != nil {
			return grepSemanticEncoded{}, fmt.Errorf("%s: %w", T("grep.semantic.backend_error"), err)
		}
		for i, vector := range encoded {
			vectors[items[start+i].index] = vector
		}
		if end < len(items) && time.Since(lastProgress) >= 5*time.Second {
			fmt.Fprintln(progress, T("grep.model.embedding_progress", end, len(items)))
			lastProgress = time.Now()
		}
	}
	return grepSemanticEncoded{Revision: grepMiniLMRevision + ":onnx-" + grepRuntimeVersion + ":wordpiece-v1:chunk-content-v1", Vectors: vectors}, nil
}

func grepSemanticRoot(paths []string) (string, error) {
	if len(paths) != 1 {
		return "", errors.New(T("grep.semantic.one_root"))
	}
	root, err := filepath.Abs(paths[0])
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New(T("grep.semantic.one_root"))
	}
	return filepath.EvalSymlinks(root)
}

var grepSemanticCacheRoot string

func grepSemanticPath(root string) (string, error) {
	base := grepSemanticCacheRoot
	var err error
	if base == "" {
		base, err = os.UserCacheDir()
		if err != nil {
			return "", err
		}
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(base, "edc", "semantic", hex.EncodeToString(sum[:])+".gob"), nil
}

func grepSemanticKey(root string, chunk grepAskChunk) string {
	absolute, _ := filepath.Abs(chunk.result.Path)
	path, _ := filepath.Rel(root, absolute)
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00", filepath.ToSlash(path))
	io.WriteString(h, strings.Join(chunk.lines, "\n"))
	return hex.EncodeToString(h.Sum(nil))
}

func grepSemanticRead(path, root string) (*grepSemanticIndex, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var header [36]byte
	if _, err := io.ReadFull(file, header[:]); err != nil || string(header[:4]) != "EDC1" {
		return nil, errors.New(T("grep.semantic.invalid"))
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, (256<<20)+1))
	if err != nil || n > 256<<20 || !bytes.Equal(header[4:], hash.Sum(nil)) {
		return nil, errors.New(T("grep.semantic.invalid"))
	}
	if _, err := file.Seek(36, io.SeekStart); err != nil {
		return nil, err
	}
	var index grepSemanticIndex
	if err := gob.NewDecoder(io.LimitReader(file, 256<<20)).Decode(&index); err != nil {
		return nil, errors.New(T("grep.semantic.invalid"))
	}
	if index.Version != 1 || index.Root != root || index.Model != grepSemanticModel || index.Revision == "" || index.Vectors == nil {
		return nil, errors.New(T("grep.semantic.invalid"))
	}
	for _, v := range index.Vectors {
		if len(v) != 384 {
			return nil, errors.New(T("grep.semantic.invalid"))
		}
		for _, x := range v {
			if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
				return nil, errors.New(T("grep.semantic.invalid"))
			}
		}
	}
	return &index, nil
}

func grepSemanticSave(path string, index *grepSemanticIndex) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".semantic-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(make([]byte, 36)); err != nil {
		f.Close()
		return err
	}
	hash := sha256.New()
	if err := gob.NewEncoder(io.MultiWriter(f, hash)).Encode(index); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append([]byte("EDC1"), hash.Sum(nil)...)); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func grepSemanticLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(file.Fd()), syscall.LOCK_UN); file.Close() }, nil
}

func grepSemanticSync(root string, chunks []grepAskChunk, index *grepSemanticIndex, query string, progress io.Writer) ([]float32, bool, error) {
	texts := []string{}
	if query != "" {
		texts = append(texts, query)
	}
	missing := []string{}
	active := map[string]bool{}
	queued := map[string]bool{}
	for _, chunk := range chunks {
		key := grepSemanticKey(root, chunk)
		active[key] = true
		if _, ok := index.Vectors[key]; !ok && !queued[key] {
			queued[key] = true
			absolute, _ := filepath.Abs(chunk.result.Path)
			path, _ := filepath.Rel(root, absolute)
			text := strings.Join(chunk.lines, "\n")
			if runes := []rune(text); len(runes) > 3000 {
				text = string(runes[:3000])
			}
			texts = append(texts, filepath.ToSlash(path)+"\n"+text)
			missing = append(missing, key)
		}
	}
	if len(texts) == 0 {
		texts = append(texts, "initialize")
	}
	encoded, err := grepSemanticEncode(texts, progress)
	if err != nil {
		return nil, false, err
	}
	if encoded.Revision == "" || (index.Revision != "" && index.Revision != encoded.Revision) {
		return nil, false, errors.New(T("grep.semantic.invalid"))
	}
	index.Revision = encoded.Revision
	vectors := encoded.Vectors
	if len(vectors) != len(texts) {
		return nil, false, errors.New("embedding count mismatch")
	}
	changed := len(missing) > 0 || len(index.Vectors) != len(active)
	offset := 0
	var queryVector []float32
	if query != "" {
		queryVector = vectors[0]
		offset = 1
	} else if len(missing) == 0 {
		offset = 1
	}
	for i, key := range missing {
		index.Vectors[key] = vectors[i+offset]
	}
	for key := range index.Vectors {
		if !active[key] {
			delete(index.Vectors, key)
		}
	}
	return queryVector, changed, nil
}

func grepSemanticVerify(root string, chunks []grepAskChunk, options grepOptions) error {
	current, _, problems := grepAskCollect([]string{root}, options)
	if len(problems) > 0 {
		return fmt.Errorf("%s: %s", T("grep.semantic.changed"), strings.Join(problems, "; "))
	}
	if len(current) != len(chunks) {
		return errors.New(T("grep.semantic.changed"))
	}
	type snapshotKey struct {
		content    string
		start, end int
	}
	seen := make(map[snapshotKey]int, len(chunks))
	for _, chunk := range chunks {
		seen[snapshotKey{grepSemanticKey(root, chunk), chunk.result.Start, chunk.result.End}]++
	}
	for _, chunk := range current {
		key := snapshotKey{grepSemanticKey(root, chunk), chunk.result.Start, chunk.result.End}
		seen[key]--
		if seen[key] < 0 {
			return errors.New(T("grep.semantic.changed"))
		}
	}
	return nil
}

func grepSemanticRank(paths []string, options grepOptions, chunks []grepAskChunk, stderr io.Writer) ([]grepAskResult, error) {
	root, err := grepSemanticRoot(paths)
	if err != nil {
		return nil, err
	}
	path, err := grepSemanticPath(root)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("%s: %w", T("grep.semantic.init_required"), err)
	}
	unlock, err := grepSemanticLock(path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	index, err := grepSemanticRead(path, root)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(stderr, T("grep.semantic.loading"))
	query, changed, err := grepSemanticSync(root, chunks, index, options.pattern, stderr)
	if err != nil {
		return nil, err
	}
	if err := grepSemanticVerify(root, chunks, options); err != nil {
		return nil, err
	}
	if changed {
		if err := grepSemanticSave(path, index); err != nil {
			return nil, err
		}
	}
	return grepSemanticScore(root, options, chunks, index, query), nil
}

func grepSemanticScore(root string, options grepOptions, chunks []grepAskChunk, index *grepSemanticIndex, query []float32) []grepAskResult {
	type chunkID struct {
		path       string
		start, end int
	}
	keywordResults := grepAskScore(options.pattern, chunks)
	keywordScores := make(map[chunkID]float64, len(keywordResults))
	maxKeyword := 0.0
	for _, result := range keywordResults {
		keywordScores[chunkID{result.Path, result.Start, result.End}] = result.Score
		if result.Score > maxKeyword {
			maxKeyword = result.Score
		}
	}
	results := make([]grepAskResult, 0, len(chunks))
	for _, chunk := range chunks {
		vector := index.Vectors[grepSemanticKey(root, chunk)]
		score := 0.0
		for i, x := range query {
			score += float64(x) * float64(vector[i])
		}
		result := chunk.result
		result.Score = 0.95 * score
		if maxKeyword > 0 {
			result.Score += 0.05 * keywordScores[chunkID{result.Path, result.Start, result.End}] / maxKeyword
		}
		lines := append([]string(nil), chunk.lines[:min(3, len(chunk.lines))]...)
		for i, line := range lines {
			runes := []rune(line)
			if len(runes) > 300 {
				lines[i] = string(runes[:300]) + "…"
			}
		}
		result.Snippet = strings.Join(lines, "\n")
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		if results[i].Path != results[j].Path {
			return results[i].Path < results[j].Path
		}
		return results[i].Start < results[j].Start
	})
	selected := make([]grepAskResult, 0, min(options.askLimit, len(results)))
	counts := map[string]int{}
	for _, r := range results {
		if counts[r.Path] >= 2 {
			continue
		}
		selected = append(selected, r)
		counts[r.Path]++
		if len(selected) == options.askLimit {
			break
		}
	}
	return selected
}

func grepSemanticLifecycle(args []string, stdout, stderr io.Writer) int {
	return grepSemanticLifecycleWithInput(args, os.Stdin, isTerminal(os.Stdin), stdout, stderr)
}

func grepSemanticLifecycleWithInput(args []string, input io.Reader, terminal bool, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return 2
	}
	rootArg := "."
	rootSet, yes := false, false
	for _, arg := range args[1:] {
		if arg == "--yes" {
			if yes || args[0] != "--semantic-init" {
				fmt.Fprintln(stderr, T("grep.semantic.usage"))
				return 2
			}
			yes = true
			continue
		}
		if strings.HasPrefix(arg, "-") || rootSet || args[0] == "--model-clear" {
			fmt.Fprintln(stderr, T("grep.semantic.usage"))
			return 2
		}
		rootArg, rootSet = arg, true
	}
	if args[0] == "--model-clear" {
		dir, err := grepModelDir()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if _, err := os.Lstat(dir); os.IsNotExist(err) {
			fmt.Fprintln(stdout, T("grep.model.cleared"))
			return 0
		} else if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		unlock, err := grepModelLock()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		defer unlock()
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		fmt.Fprintln(stdout, T("grep.model.cleared"))
		return 0
	}
	root, err := grepSemanticRoot([]string{rootArg})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	path, err := grepSemanticPath(root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if args[0] == "--semantic-status" {
		dir, modelErr := grepModelDir()
		if modelErr == nil {
			modelErr = grepModelCheck(dir)
		}
		if modelErr != nil {
			fmt.Fprintln(stdout, T("grep.model.status_missing"))
		} else {
			fmt.Fprintln(stdout, T("grep.model.status_ready", dir))
		}
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			fmt.Fprintln(stderr, T("grep.semantic.init_required"))
			return 1
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		index, err := grepSemanticRead(path, root)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		fmt.Fprintf(stdout, "%s @ %s: %d chunks, %s (%d bytes)\n", index.Model, index.Revision, len(index.Vectors), path, info.Size())
		return 0
	}
	if args[0] == "--semantic-clear" {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			fmt.Fprintln(stdout, T("grep.semantic.cleared"))
			return 0
		} else if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		unlock, err := grepSemanticLock(path)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		defer unlock()
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(stderr, err)
			return 2
		}
		fmt.Fprintln(stdout, T("grep.semantic.cleared"))
		return 0
	}
	chunks, skipped, problems := grepAskCollect([]string{root}, grepOptions{})
	if len(problems) > 0 {
		for _, problem := range problems {
			fmt.Fprintln(stderr, T("grep.failed", problem))
		}
		return 2
	}
	if skipped > 0 {
		fmt.Fprintln(stderr, T("grep.ask.skipped", skipped))
	}
	if err := grepSemanticEnsure(input, terminal, yes, stderr); err != nil {
		if errors.Is(err, errGrepModelCancelled) {
			fmt.Fprintln(stderr, T("grep.model.cancelled"))
			return 4
		}
		fmt.Fprintln(stderr, err)
		return 2
	}
	unlock, err := grepSemanticLock(path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	defer unlock()
	index := &grepSemanticIndex{Version: 1, Root: root, Model: grepSemanticModel, Vectors: map[string][]float32{}}

	fmt.Fprintln(stderr, T("grep.semantic.loading"))
	if _, _, err = grepSemanticSync(root, chunks, index, "", stderr); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err = grepSemanticVerify(root, chunks, grepOptions{}); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err = grepSemanticSave(path, index); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	fmt.Fprintf(stdout, "%s: %d chunks, %s\n", grepSemanticModel, len(chunks), path)
	return 0
}
