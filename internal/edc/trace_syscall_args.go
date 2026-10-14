package edc

import (
	"fmt"
	"strings"
)

// syscallPathArgs는 경로를 받는 syscall과 그 경로가 몇 번째 인자인지다(0부터). 경로가 둘인 호출은 첫 경로다.
// amd64에만 있는 open, stat 같은 이름은 arm64에서 번호가 없어 건너뛴다.
var syscallPathArgs = map[string]uint8{
	"open": 0, "creat": 0, "openat": 1, "openat2": 1,
	"stat": 0, "lstat": 0, "newfstatat": 1, "statx": 1, "statfs": 0,
	"access": 0, "faccessat": 1, "faccessat2": 1,
	"readlink": 0, "readlinkat": 1,
	"unlink": 0, "unlinkat": 1, "rmdir": 0, "mkdir": 0, "mkdirat": 1, "mknod": 0, "mknodat": 1,
	"rename": 0, "renameat": 1, "renameat2": 1, "link": 0, "linkat": 1, "symlink": 1, "symlinkat": 2,
	"chdir": 0, "chroot": 0, "chmod": 0, "fchmodat": 1, "fchmodat2": 1, "chown": 0, "lchown": 0, "fchownat": 1,
	"truncate": 0, "utimes": 0, "utimensat": 1, "futimesat": 1,
	"execve": 0, "execveat": 1,
	"getxattr": 0, "lgetxattr": 0, "setxattr": 0, "lsetxattr": 0, "listxattr": 0, "llistxattr": 0, "removexattr": 0, "lremovexattr": 0,
	"inotify_add_watch": 1, "mount": 1, "umount2": 0, "swapon": 0, "pivot_root": 0,
}

// syscallPathGone은 성공하면 경로를 읽을 수 없는 syscall이다. execve가 성공하면 register와 memory가 새 program의 것이다.
var syscallPathGone = map[string]bool{"execve": true, "execveat": true}

// parseSyscallNames는 --syscall의 이름을 번호로 바꾼다. amd64에서는 32비트 호환 번호도 고른다. 한쪽 표에만 있는
// 이름도 받고, 어느 표에도 없는 이름은 오류다.
func parseSyscallNames(arch, value string) (native, compat []uint32, err error) {
	for _, name := range strings.Split(value, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			return nil, nil, errTopProcessFilterEmpty
		}
		found := false
		if nr, ok := syscallNumber(arch, name); ok {
			native, found = append(native, nr), true
		}
		if arch == "amd64" {
			if nr, ok := syscallNumber("386", name); ok {
				compat, found = append(compat, nr), true
			}
		}
		if !found {
			return nil, nil, fmt.Errorf("unknown system call %q on %s", name, arch)
		}
	}
	return native, compat, nil
}
