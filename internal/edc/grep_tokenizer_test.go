package edc

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGrepTokenizerMatchesMiniLMExamples(t *testing.T) {
	vocab := map[string]int64{"[PAD]": 0, "[UNK]": 100, "[CLS]": 101, "[SEP]": 102, "[MASK]": 103, "hello": 7592, "world": 2088, "valid": 9398, "##ate": 3686, "##tok": 18715, "##en": 2368, ":": 1024, "token": 19204, ".": 1012, "au": 8740, "##th": 2705, "(": 1006, "ct": 14931, "##x": 2595, ")": 1007, "cafe": 7668, "ᄉ": 1461, "##ᅥ": 30008, "##ᄋ": 29999, "##ᅮ": 30014, "##ᆯ": 30022, "https": 16770, "##sta": 9153, "##tus": 5809, "##code": 16044, "=": 1027, "_": 1035, "id": 8909, "test": 3231, "end": 2203, ",": 1010}
	tokenizer := grepTokenizer{vocab: vocab}
	for _, tc := range []struct {
		text string
		want []int64
	}{
		{"hello world", []int64{101, 7592, 2088, 102}},
		{"ValidateToken: token.Auth(ctx)", []int64{101, 9398, 3686, 18715, 2368, 1024, 19204, 1012, 8740, 2705, 1006, 14931, 2595, 1007, 102}},
		{"Café [MASK] 서울 🌍", []int64{101, 7668, 103, 1461, 30008, 29999, 30014, 30022, 100, 102}},
		{"HTTPSStatusCode=TOKEN_ID", []int64{101, 16770, 9153, 5809, 16044, 1027, 19204, 1035, 8909, 102}},
		{"\x00test\u200b end", []int64{101, 3231, 2203, 102}},
		{"你好,world", []int64{101, 100, 100, 1010, 2088, 102}},
	} {
		if got := tokenizer.encode(tc.text); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q got %v want %v", tc.text, got, tc.want)
		}
	}
	got := tokenizer.encode(strings.Repeat("hello ", 200))
	if len(got) != 128 || got[0] != 101 || got[127] != 102 {
		t.Fatalf("truncation %v", got)
	}
}

func TestGrepTokenizerRejectsInvalidVocabulary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vocab.txt")
	var lines []string
	for i := 0; i < 104; i++ {
		lines = append(lines, fmt.Sprintf("unused%d", i))
	}
	for id, word := range map[int]string{0: "[PAD]", 100: "[UNK]", 101: "[CLS]", 102: "[SEP]", 103: "[MASK]"} {
		lines[id] = word
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newGrepTokenizer(path); err != nil {
		t.Fatal(err)
	}
	lines[0] = "broken"
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newGrepTokenizer(path); err == nil {
		t.Fatal("invalid vocabulary accepted")
	}
}
