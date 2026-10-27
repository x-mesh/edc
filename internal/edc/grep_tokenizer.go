package edc

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

type grepTokenizer struct{ vocab map[string]int64 }

func newGrepTokenizer(path string) (*grepTokenizer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	vocab := make(map[string]int64, 30522)
	scanner := bufio.NewScanner(f)
	var id int64
	for scanner.Scan() {
		word := scanner.Text()
		if _, exists := vocab[word]; exists {
			return nil, fmt.Errorf("duplicate tokenizer token %q", word)
		}
		vocab[word] = id
		id++
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for word, want := range map[string]int64{"[PAD]": 0, "[UNK]": 100, "[CLS]": 101, "[SEP]": 102, "[MASK]": 103} {
		if got, ok := vocab[word]; !ok || got != want {
			return nil, fmt.Errorf("invalid tokenizer %s", word)
		}
	}
	return &grepTokenizer{vocab: vocab}, nil
}

func grepChinese(r rune) bool {
	return r >= 0x4e00 && r <= 0x9fff || r >= 0x3400 && r <= 0x4dbf || r >= 0x20000 && r <= 0x2a6df || r >= 0x2a700 && r <= 0x2b73f || r >= 0x2b740 && r <= 0x2b81f || r >= 0x2b820 && r <= 0x2ceaf || r >= 0xf900 && r <= 0xfaff || r >= 0x2f800 && r <= 0x2fa1f
}
func grepPunctuation(r rune) bool {
	return r >= 33 && r <= 47 || r >= 58 && r <= 64 || r >= 91 && r <= 96 || r >= 123 && r <= 126 || unicode.IsPunct(r)
}

func (t *grepTokenizer) encode(text string) []int64 {
	ids := make([]int64, 0, 128)
	ids = append(ids, 101)
	appendWord := func(word string) {
		if word == "" || len(ids) >= 127 {
			return
		}
		runes := []rune(word)
		if len(runes) > 100 {
			ids = append(ids, 100)
			return
		}
		pieces := make([]int64, 0, 4)
		for start := 0; start < len(runes); {
			match := int64(-1)
			next := start
			for end := len(runes); end > start; end-- {
				p := string(runes[start:end])
				if start > 0 {
					p = "##" + p
				}
				if id, ok := t.vocab[p]; ok {
					match = id
					next = end
					break
				}
			}
			if match < 0 {
				ids = append(ids, 100)
				return
			}
			pieces = append(pieces, match)
			start = next
		}
		for _, piece := range pieces {
			if len(ids) >= 127 {
				break
			}
			ids = append(ids, piece)
		}
	}
	normal := func(part string) {
		var word strings.Builder
		flush := func() { appendWord(word.String()); word.Reset() }
		for _, r := range norm.NFD.String(strings.ToLower(part)) {
			if r == 0 || r == utf8.RuneError || ((unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) && r != '\t' && r != '\n' && r != '\r') {
				continue
			}
			if unicode.Is(unicode.Mn, r) {
				continue
			}
			if unicode.IsSpace(r) {
				flush()
				continue
			}
			if grepPunctuation(r) || grepChinese(r) {
				flush()
				appendWord(string(r))
				continue
			}
			word.WriteRune(r)
		}
		flush()
	}
	specials := []string{"[PAD]", "[UNK]", "[CLS]", "[SEP]", "[MASK]"}
	for len(text) > 0 && len(ids) < 127 {
		next := len(text)
		special := ""
		for _, item := range specials {
			if i := strings.Index(text, item); i >= 0 && i < next {
				next = i
				special = item
			}
		}
		normal(text[:next])
		if special == "" {
			break
		}
		if len(ids) < 127 {
			ids = append(ids, t.vocab[special])
		}
		text = text[next+len(special):]
	}
	ids = append(ids, 102)
	return ids
}
