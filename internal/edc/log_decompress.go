package edc

import (
	"io"
	"strings"

	"github.com/x-mesh/fastcompress/gzip"
	"github.com/x-mesh/fastcompress/zstd"
)

// logDecompressor는 회전 로그의 확장자로 푸는 함수를 고른다. .gz와 .zst가 아니면 nil이다.
func logDecompressor(path string) func(io.Reader) (io.ReadCloser, error) {
	switch {
	case strings.HasSuffix(path, ".gz"):
		return func(src io.Reader) (io.ReadCloser, error) {
			reader, err := gzip.NewReader(src)
			if err != nil {
				return nil, err
			}
			return reader, nil
		}
	case strings.HasSuffix(path, ".zst"):
		return func(src io.Reader) (io.ReadCloser, error) {
			reader, err := zstd.NewReader(src)
			if err != nil {
				return nil, err
			}
			return reader, nil
		}
	}
	return nil
}
