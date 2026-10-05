package edc

import (
	"strings"
	"testing"
)

func TestLogCommandKeyPreservesArgumentIdentity(t *testing.T) {
	commands := [][]string{{"ls"}, {"ls", "-l"}, {"ls", "/tmp"}, {"echo", "a b"}, {"echo", "a", "b"}, {"echo", ""}, {"echo", "秘密"}, {"ls", "-a", "-l"}, {"ls", "-l", "-a"}, {"/bin/ls"}}
	seen := map[string]bool{}
	for _, argv := range commands {
		key, err := commandKey(argv)
		if err != nil || !validHistoryKey(key) || seen[key] {
			t.Fatalf("argv %q key %q err %v", argv, key, err)
		}
		seen[key] = true
		repeated, err := commandKey(append([]string{}, argv...))
		if err != nil || key != repeated {
			t.Fatalf("unstable identity for %q", argv)
		}
	}
	for _, argv := range [][]string{nil, {}, {""}, {"echo", string([]byte{0xff})}} {
		if _, err := commandKey(argv); err == nil {
			t.Fatalf("invalid argv accepted: %q", argv)
		}
	}
	for _, argv := range [][]string{{"echo", "a b"}, {"echo", "秘密"}} {
		key, _ := commandKey(argv)
		if strings.Contains(key, argv[1]) {
			t.Fatalf("argument in key: %q", key)
		}
	}
}
