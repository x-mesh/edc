package edc

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func findRank(t *testing.T, query string, paths []string) []string {
	t.Helper()
	matcher, _, err := newFindMatcher(query, false)
	if err != nil {
		t.Fatal(err)
	}
	type hit struct {
		path  string
		score int
	}
	var hits []hit
	for _, path := range paths {
		if score, ok := matcher.score(path, findBase(path)); ok {
			hits = append(hits, hit{path, score})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].score > hits[b].score })
	ranked := make([]string, 0, len(hits))
	for _, hit := range hits {
		ranked = append(ranked, hit.path)
	}
	return ranked
}

func TestFindFuzzyRanksNamesAboveDirectories(t *testing.T) {
	for _, tc := range []struct {
		query string
		paths []string
		first string
	}{
		// 이 호스트의 / 목록에서 고른 경로다. 파일 이름이 맞는 것이 디렉터리에서 맞는 것보다 위다.
		{"bashrc", []string{"/usr/share/doc/bash/bashrc.gz.d/x", "/etc/skel/.bashrc", "/root/.bashrc", "/usr/share/bash-completion/brc"}, "/root/.bashrc"},
		{"adjrn", []string{"internal/edc/audit_journal.go", "internal/edc/admin/journal.go", "android/jar/run.txt"}, "internal/edc/audit_journal.go"},
		{"nginxconf", []string{"/etc/nginx/conf.d/default.conf", "/etc/nginx/nginx.conf", "/etc/nginx/fastcgi.conf"}, "/etc/nginx/nginx.conf"},
		{"sshdconfig", []string{"/usr/share/vim/vim91/syntax/sshdconfig.vim", "/etc/ssh/sshd_config", "/etc/ssh/sshd_config.d"}, "/etc/ssh/sshd_config"},
		// 단어 경계에서 맞는 쪽이 위다.
		{"ft", []string{"docs/left.md", "docs/find_test.go"}, "docs/find_test.go"},
	} {
		ranked := findRank(t, tc.query, tc.paths)
		if len(ranked) == 0 || ranked[0] != tc.first {
			t.Errorf("%s: ranked = %v", tc.query, ranked)
		}
	}
}

func TestFindFuzzyNeedsEveryTermInOrder(t *testing.T) {
	paths := []string{"etc/nginx/nginx.conf", "etc/apache2/apache2.conf", "var/log/nginx/access.log"}
	if ranked := findRank(t, "nginx conf", paths); !reflect.DeepEqual(ranked, []string{"etc/nginx/nginx.conf"}) {
		t.Errorf("two terms = %v", ranked)
	}
	// 글자는 순서대로 있어야 한다.
	if ranked := findRank(t, "fnoc", paths); len(ranked) != 0 {
		t.Errorf("reversed = %v", ranked)
	}
	if ranked := findRank(t, "", paths); len(ranked) != 3 {
		t.Errorf("empty = %v", ranked)
	}
}

func TestFindFuzzySmartCase(t *testing.T) {
	paths := []string{"src/Makefile", "src/makefile.old"}
	if ranked := findRank(t, "make", paths); len(ranked) != 2 {
		t.Errorf("lower = %v", ranked)
	}
	if ranked := findRank(t, "Make", paths); !reflect.DeepEqual(ranked, []string{"src/Makefile"}) {
		t.Errorf("upper = %v", ranked)
	}
}

func TestFindFuzzyComparesRunes(t *testing.T) {
	// "한"(ED 95 9C)의 바이트를 "핸"(ED 95 B8)과 "솜"(EC 86 9C)에서 하나씩 골라 맞추면 안 된다.
	paths := []string{"docs/핸솜.md", "docs/한글.md", "docs/KELVIN.txt"}
	if ranked := findRank(t, "한", paths); !reflect.DeepEqual(ranked, []string{"docs/한글.md"}) {
		t.Errorf("hangul = %v", ranked)
	}
	// İ(U+0130, 2바이트)는 소문자로 i(1바이트)가 된다. 끝 글자의 크기를 질의 쪽에서 재면 구간이 어긋난다.
	if ranked := findRank(t, "한i", []string{"docs/한\u0130.txt"}); len(ranked) != 1 {
		t.Errorf("dotted I = %v", ranked)
	}
	// σ와 끝 시그마 ς는 소문자끼리지만 같은 대소문자 접기에 든다.
	if ranked := findRank(t, "σ", []string{"docs/ς.txt", "docs/Σ.txt"}); len(ranked) != 2 {
		t.Errorf("sigma = %v", ranked)
	}
	matcher, _, _ := newFindMatcher("글", false)
	path := "docs/한글.md"
	if spans := matcher.spans(path, findBase(path)); !reflect.DeepEqual(spans, [][2]int{{8, 11}}) {
		t.Errorf("spans = %v", spans)
	}
}

func TestFindMatcherSpansCoverTheMatchedLetters(t *testing.T) {
	matcher, _, _ := newFindMatcher("nginx conf", false)
	path := "etc/nginx/nginx.conf"
	spans := matcher.spans(path, findBase(path))
	var parts []string
	for _, span := range spans {
		parts = append(parts, path[span[0]:span[1]])
	}
	// 파일 이름 쪽 구간이 점수가 더 높아 그쪽을 강조한다.
	if strings.Join(parts, "|") != "nginx|conf" || spans[0][0] != 10 {
		t.Errorf("spans = %v %v", spans, parts)
	}
}

func TestFindRegexMatcher(t *testing.T) {
	matcher, _, err := newFindMatcher(`\.conf$`, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := matcher.score("etc/NGINX.CONF", 4); !ok || matcher.ranked() {
		t.Errorf("a lower-case pattern must ignore case and keep path order")
	}
	if _, ok := matcher.score("etc/nginx.conf.bak", 4); ok {
		t.Error("anchored pattern matched")
	}
	if _, _, err := newFindMatcher("(", true); err == nil {
		t.Error("a broken pattern must fail")
	}
}

func TestFindMatcherCutsALongQuery(t *testing.T) {
	query := strings.Repeat("가", 100)
	matcher, truncated, err := newFindMatcher(query, false)
	if err != nil || !truncated {
		t.Fatalf("truncated %v, err %v", truncated, err)
	}
	if term := matcher.(findFuzzy).terms[0].text; len(term) > findQueryLimit || !strings.HasPrefix(query, term) {
		t.Errorf("term = %d bytes", len(term))
	}
}

// findBenchPaths는 / 목록과 비슷한 깊이와 이름을 가진 합성 경로다.
func findBenchPaths(count int) []string {
	dirs := []string{"usr", "share", "lib", "python3", "dist-packages", "var", "cache", "etc", "systemd", "system", "locale", "doc", "include", "linux", "go", "src", "internal", "pkg", "mod", "github.com"}
	names := []string{"journal", "config", "nginx", "readme", "main", "index", "test", "util", "conf", "service", "handler", "client"}
	exts := []string{".go", ".py", ".conf", ".json", ".gz", ".h", ".so", ".md"}
	paths := make([]string, count)
	for index := range paths {
		var builder strings.Builder
		depth := 3 + index%6
		for level := 0; level < depth; level++ {
			builder.WriteByte('/')
			builder.WriteString(dirs[(index/(level+1)+level*7)%len(dirs)])
		}
		fmt.Fprintf(&builder, "/%s_%d%s", names[index%len(names)], index%97, exts[index%len(exts)])
		paths[index] = builder.String()
	}
	return paths
}

func BenchmarkFindFuzzy(b *testing.B) {
	paths := findBenchPaths(800_000)
	for _, query := range []string{"e", "nginxconf", "sysjrn"} {
		matcher, _, _ := newFindMatcher(query, false)
		b.Run(query, func(b *testing.B) {
			for b.Loop() {
				if hits := findScore(matcher, paths, nil, 0); len(hits) > len(paths) {
					b.Fatal(len(hits))
				}
			}
		})
	}
}
