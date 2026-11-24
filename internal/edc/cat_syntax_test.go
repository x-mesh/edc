package edc

import (
	"strings"
	"testing"
)

// catDescribe는 조각을 "<k:KEY>=<l:1>"처럼 적는다. 평문 조각은 그대로 둔다.
func catDescribe(spans []catSpan) string {
	tags := map[catClass]string{catKey: "k", catString: "s", catLiteral: "l", catValue: "v", catComment: "c", catSection: "h", catMasked: "m"}
	var builder strings.Builder
	for _, span := range spans {
		tag, ok := tags[span.class]
		if !ok {
			builder.WriteString(span.text)
			continue
		}
		builder.WriteString("<" + tag + ":" + span.text + ">")
	}
	return builder.String()
}

func catLexLines(format catFormat, mask bool, lines ...string) []string {
	lexer := catLexer{format: format, mask: mask}
	described := make([]string, 0, len(lines))
	for _, line := range lines {
		described = append(described, catDescribe(lexer.line(line)))
	}
	return described
}

func TestDetectCatFormat(t *testing.T) {
	for path, want := range map[string]catFormat{
		".env":                          catFormatEnv,
		"deploy/.env.production":        catFormatEnv,
		"prod.env":                      catFormatEnv,
		"package.json":                  catFormatJSON,
		"events.jsonl":                  catFormatJSON,
		"values.YAML":                   catFormatYAML,
		"compose.yml":                   catFormatYAML,
		"Cargo.toml":                    catFormatTOML,
		"my.cnf":                        catFormatINI,
		"/etc/systemd/system/a.service": catFormatINI,
		"app.properties":                catFormatINI,
		"README.md":                     catFormatPlain,
		"environment":                   catFormatPlain,
		"main.go":                       catFormatPlain,
	} {
		if got := detectCatFormat(path); got != want {
			t.Errorf("detectCatFormat(%q) = %s, want %s", path, got.name(), want.name())
		}
	}
}

func TestCatLexerClassifiesEachFormat(t *testing.T) {
	for _, test := range []struct {
		name   string
		format catFormat
		lines  []string
		want   []string
	}{
		{"env", catFormatEnv,
			[]string{"# note", "export API_URL=http://localhost:8080 # dev", "DEBUG=true", `NAME="a # b" # c`, "EMPTY=", "not a pair"},
			[]string{"<c:# note>", "export <k:API_URL>=<v:http://localhost:8080> <c:# dev>", "<k:DEBUG>=<l:true>", `<k:NAME>=<s:"a # b"> <c:# c>`, "<k:EMPTY>=", "<v:not a pair>"}},
		{"json", catFormatJSON,
			[]string{`{"name": "edc", "n": -1.5e3, "ok": null}`, `  "list": ["a\"b", true]`},
			[]string{`{<k:"name">: <s:"edc">, <k:"n">: <l:-1.5e3>, <k:"ok">: <l:null>}`, `  <k:"list">: [<s:"a\"b">, <l:true>]`}},
		{"yaml", catFormatYAML,
			[]string{"---", "server:", "  host: example.com # prod", "  url: http://a:80/p", "  - name: 'it''s'", `  "quoted key": 3`, "  - plain item", "# note"},
			[]string{"<h:--->", "<k:server>:", "  <k:host>: <v:example.com> <c:# prod>", "  <k:url>: <v:http://a:80/p>", "  - <k:name>: <s:'it''s'>", `  <k:"quoted key">: <l:3>`, "  - <v:plain item>", "<c:# note>"}},
		{"toml", catFormatTOML,
			[]string{"[owner] # who", `name = "Tom" # c`, `"a=b" = 0x1F`, `point = { x = 1, y = 'a' }`},
			[]string{"<h:[owner]> <c:# who>", `<k:name> = <s:"Tom"> <c:# c>`, `<k:"a=b"> = <l:0x1F>`, "<k:point> = { <k:x> = <l:1>, <k:y> = <s:'a'> }"}},
		{"ini", catFormatINI,
			[]string{"[Service]", "; note", "ExecStart=/usr/bin/app --port=80", `name = "x"`, "skip-networking"},
			[]string{"<h:[Service]>", "<c:; note>", "<k:ExecStart>=<v:/usr/bin/app --port=80>", `<k:name> = <s:"x">`, "<v:skip-networking>"}},
		{"plain", catFormatPlain, []string{"key=value # text"}, []string{"key=value # text"}},
	} {
		got := catLexLines(test.format, false, test.lines...)
		for index := range test.want {
			if got[index] != test.want[index] {
				t.Errorf("%s line %q:\n got %s\nwant %s", test.name, test.lines[index], got[index], test.want[index])
			}
		}
	}
}

// 여러 줄에 걸친 값 안의 줄은 키나 주석처럼 보여도 값이다.
func TestCatLexerCarriesOpenValuesAcrossLines(t *testing.T) {
	for _, test := range []struct {
		name   string
		format catFormat
		lines  []string
		want   []string
	}{
		{"env quoted", catFormatEnv,
			[]string{`KEY="-----BEGIN`, "QUJD=", "# inside", `-----END" # done`, "AFTER=1"},
			[]string{`<k:KEY>=<s:"-----BEGIN>`, "<s:QUJD=>", "<s:# inside>", `<s:-----END"> <c:# done>`, "<k:AFTER>=<l:1>"}},
		{"yaml block scalar", catFormatYAML,
			[]string{"  cert: |-", "    user: admin", "", "    # inside", "  next: 1", "- >", "  folded: text", "- done"},
			[]string{"  <k:cert>: |-", "    <s:user: admin>", "", "    <s:# inside>", "  <k:next>: <l:1>", "- >", "  <s:folded: text>", "- <v:done>"}},
		{"yaml tagged block scalar", catFormatYAML,
			[]string{"data: !!binary |", "  # inside", "after: x"},
			[]string{"<k:data>: !!binary |", "  <s:# inside>", "<k:after>: <v:x>"}},
		{"yaml quoted", catFormatYAML,
			[]string{`text: "first`, "  # inside", `  last" # done`, "after: x"},
			[]string{`<k:text>: <s:"first>`, "<s:  # inside>", `<s:  last"> <c:# done>`, "<k:after>: <v:x>"}},
		{"ini backslash", catFormatINI,
			[]string{`ExecStart=/bin/app \`, `  --flag=1 \`, "# inside", "After=x"},
			[]string{`<k:ExecStart>=<v:/bin/app \>`, `  <v:--flag=1 \>`, "<v:# inside>", "<k:After>=<v:x>"}},
		{"toml string and array", catFormatTOML,
			[]string{`text = """`, "abc = def", `# inside"""`, "ports = [", "  8001, # first", "]", "on = true"},
			[]string{`<k:text> = <s:""">`, "<s:abc = def>", `<s:# inside""">`, "<k:ports> = [", "  <l:8001>, <c:# first>", "]", "<k:on> = <l:true>"}},
	} {
		got := catLexLines(test.format, false, test.lines...)
		for index := range test.want {
			if got[index] != test.want[index] {
				t.Errorf("%s line %q:\n got %s\nwant %s", test.name, test.lines[index], got[index], test.want[index])
			}
		}
	}
}

func TestCatMaskHidesEveryValue(t *testing.T) {
	const mask = "<m:" + catMaskText + ">"
	for _, test := range []struct {
		name   string
		format catFormat
		lines  []string
		want   []string
	}{
		{"env", catFormatEnv,
			[]string{"# note", "TOKEN=s3cr3t # s3cr3t", `KEY="s3cr3t`, "s3cr3t=", `# s3cr3t" s3cr3t`, "EMPTY=", "s3cr3t without a pair"},
			[]string{"<c:# note>", "<k:TOKEN>=" + mask, "<k:KEY>=" + mask, mask, mask, "<k:EMPTY>=", mask}},
		{"json", catFormatJSON,
			[]string{`{"token": "s3cr3t", "n": 53, "list": ["s3cr3t", s3cr3t]}`},
			[]string{`{<k:"token">: ` + mask + `, <k:"n">: ` + mask + `, <k:"list">: [` + mask + ", " + mask + "]}"}},
		{"yaml", catFormatYAML,
			[]string{"token: s3cr3t # s3cr3t", "cert: |", "  s3cr3t: s3cr3t", "  # s3cr3t", `text: "s3cr3t`, `  # s3cr3t" s3cr3t`, "list:", "  - s3cr3t", "  - user: s3cr3t", "flow: [s3cr3t, s3cr3t]"},
			[]string{"<k:token>: " + mask, "<k:cert>: |", "  " + mask, "  " + mask, "<k:text>: " + mask, mask, "<k:list>:", "  - " + mask, "  - <k:user>: " + mask, "<k:flow>: " + mask}},
		{"toml", catFormatTOML,
			[]string{"[owner]", `token = "s3cr3t" # s3cr3t`, `text = """`, "s3cr3t = s3cr3t", `# s3cr3t"""`, "at = 2026-01-02 03:04:05", "list = [", `  "s3cr3t",`, "]", `point = { user = "s3cr3t" }`},
			[]string{"<h:[owner]>", "<k:token> = " + mask, "<k:text> = " + mask, mask, mask, "<k:at> = " + mask, "<k:list> = [", "  " + mask + ",", "]", "<k:point> = { <k:user> = " + mask + " }"}},
		{"ini", catFormatINI,
			[]string{"[client]", "password = s3cr3t", "s3cr3t-bare-word", "proxy_pass http://user:s3cr3t@host/?a=b;",
				`ExecStart=/bin/app \\`, "s3cr3tTail=x", "after=1", "long = first", "  s3cr3tIndented=x", "s3cr3tPad==", "s3cr3tLast="},
			[]string{"<h:[client]>", "<k:password> = " + mask, mask, mask,
				"<k:ExecStart>=" + mask, mask, "<k:after>=" + mask, "<k:long> = " + mask, "  " + mask, mask, mask}},
	} {
		got := catLexLines(test.format, true, test.lines...)
		for index := range test.want {
			if got[index] != test.want[index] {
				t.Errorf("%s line %q:\n got %s\nwant %s", test.name, test.lines[index], got[index], test.want[index])
			}
			if strings.Contains(got[index], "s3cr3t") {
				t.Errorf("%s line %q leaks a value: %s", test.name, test.lines[index], got[index])
			}
		}
	}
}
