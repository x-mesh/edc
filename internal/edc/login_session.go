package edc

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// readLinuxLoginSession은 process의 audit session ID로 logind 세션 기록을 읽는다. edc proc와 audit의 proc.remote_share가 쓴다.
// 로그인 세션 밖의 process는 ID가 4294967295이고 그 기록은 없다. 같은 디렉터리의 <id>.ref는 FIFO라서 열면 쓰는 쪽이 올
// 때까지 멈춘다. 그래서 숫자 ID의 파일 하나를, 보통 파일일 때만 연다.
func readLinuxLoginSession(root, directory string) *procLoginSession {
	data, err := os.ReadFile(filepath.Join(directory, "sessionid"))
	id := strings.TrimSpace(string(data))
	if err != nil {
		return nil
	}
	if _, err := strconv.ParseUint(id, 10, 32); err != nil {
		return nil
	}
	path := filepath.Join(root, "run/systemd/sessions", id)
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return nil
	}
	if data, err = os.ReadFile(path); err != nil {
		return nil
	}
	session := procLoginSession{ID: id}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, _ := strings.Cut(line, "=")
		switch key {
		case "USER":
			session.User = value
		case "REMOTE_HOST":
			session.RemoteHost = value
		case "SERVICE":
			session.Service = value
		case "STATE":
			session.State = value
		case "REALTIME":
			if micros, err := strconv.ParseInt(value, 10, 64); err == nil {
				session.Start = time.UnixMicro(micros)
			}
		}
	}
	return &session
}
