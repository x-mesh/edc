package edc

import "io/fs"

// macOS에는 /proc이 없어 다른 프로세스의 fd를 보지 않는다. --rm의 문서가 이 차이를 적는다.
func codecFindWriter(string, fs.FileInfo) (int, error) { return 0, nil }
