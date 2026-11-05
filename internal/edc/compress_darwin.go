package edc

import (
	"io/fs"
	"os"
)

// macOS에는 /proc도 read lease도 없어 다른 프로세스의 fd를 보지 않는다. --rm의 문서가 이 차이를 적는다.
func codecCheckWriters(*os.File, string, fs.FileInfo) error { return nil }
