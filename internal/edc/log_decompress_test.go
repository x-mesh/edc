package edc

import (
	"bytes"
	stdgzip "compress/gzip"
	"io"
	"testing"

	"github.com/x-mesh/fastcompress/zstd"
)

func TestLogDecompressorPicksTheFormatBySuffix(t *testing.T) {
	const content = "2026-11-04 10:00:00 INFO rotated\n"
	var gzipped bytes.Buffer
	writer := stdgzip.NewWriter(&gzipped)
	writer.Write([]byte(content))
	writer.Close()
	for path, data := range map[string][]byte{
		"app.log.1.gz":  gzipped.Bytes(),
		"app.log.1.zst": zstd.Encode(nil, []byte(content), 3),
	} {
		decompress := logDecompressor(path)
		if decompress == nil {
			t.Fatalf("%s: no decompressor", path)
		}
		reader, err := decompress(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		got, err := io.ReadAll(reader)
		reader.Close()
		if err != nil || string(got) != content {
			t.Fatalf("%s: got %q err %v", path, got, err)
		}
	}
	for _, path := range []string{"app.log", "app.log.1", "app.log.1.xz", "app.log.zst.bak"} {
		if logDecompressor(path) != nil {
			t.Fatalf("%s: picked a decompressor", path)
		}
	}
}
