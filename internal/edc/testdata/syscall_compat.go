// trace syscall의 32비트 호환 시험이 GOARCH=386으로 빌드해 띄운다. amd64 kernel은 이 process의 호출을 32비트
// 진입점으로 받으므로, 같은 getppid가 64비트 표와 다른 번호로 보인다.
package main

import (
	"os"
	"strconv"
	"syscall"
)

func main() {
	calls, err := strconv.Atoi(os.Args[1])
	if err != nil {
		os.Exit(2)
	}
	buffer := make([]byte, 1)
	for {
		if n, err := syscall.Read(0, buffer); n != 1 || err != nil || buffer[0] == 'x' {
			return
		}
		for range calls {
			syscall.Getppid()
		}
	}
}
