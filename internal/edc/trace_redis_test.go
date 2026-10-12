package edc

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func redisKeepAll(byte) redisKeep {
	return redisKeep{value: true, message: true, word: true, heads: true}
}

// redisFrameText는 frame을 비교하기 쉬운 한 줄로 쓴다.
func redisFrameText(f redisFrame) string {
	var out strings.Builder
	fmt.Fprintf(&out, "%c n=%d", f.kind, f.count)
	if f.null {
		out.WriteString(" null")
	}
	if f.attr {
		out.WriteString(" attr")
	}
	if f.value != "" {
		fmt.Fprintf(&out, " v=%q", f.value)
	}
	if f.word != "" {
		fmt.Fprintf(&out, " w=%q", f.word)
	}
	if f.class != "" {
		fmt.Fprintf(&out, " c=%s", f.class)
	}
	if f.message != "" {
		fmt.Fprintf(&out, " m=%q", f.message)
	}
	if f.monitor {
		out.WriteString(" monitor")
	}
	for i, head := range f.heads {
		if head.kind != 0 {
			fmt.Fprintf(&out, " h%d=%c%q", i, head.kind, head.text)
			if head.number {
				fmt.Fprintf(&out, "#%d", head.integer)
			}
		}
	}
	if c := f.command; c != nil {
		fmt.Fprintf(&out, " cmd=%q key=%q args=%d", c.name, c.key, c.args)
		if len(c.preview) > 0 {
			fmt.Fprintf(&out, " preview=%q", c.preview)
		}
		if c.sensitive {
			out.WriteString(" sensitive")
		}
		if c.proto != 0 {
			fmt.Fprintf(&out, " proto=%d", c.proto)
		}
		if c.reply != "" {
			fmt.Fprintf(&out, " reply=%s", c.reply)
		}
	}
	if f.lost {
		out.WriteString(" lost")
	}
	return out.String()
}

// redisRead는 record들을 한 reader에 차례로 먹이고 frame과 framing을 잃은 횟수를 돌려준다. size가 0이면 record 길이다.
func redisRead(reader *redisReader, records [][]byte, sizes []int) ([]string, int) {
	var frames []string
	losses := 0
	for i, record := range records {
		size := len(record)
		if i < len(sizes) && sizes[i] > 0 {
			size = sizes[i]
		}
		if !reader.feed(record, size, uint64(i+1), func(f redisFrame) { frames = append(frames, redisFrameText(f)) }) {
			losses++
		}
	}
	return frames, losses
}

func redisResponseReader() *redisReader { return &redisReader{keep: redisKeepAll} }

// redisFrameBytes는 frame들의 bytes 합이다. 건너뛴 본문까지 세어 입력 길이와 같아야 한다.
func redisFrameBytes(reader *redisReader, in string) int64 {
	var total int64
	reader.feed([]byte(in), len(in), 1, func(f redisFrame) { total += f.bytes })
	return total
}

func TestRedisReaderTypes(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"+OK\r\n", `+ n=0 v="OK" w="OK"`},
		{"-ERR unknown command 'x'\r\n", `- n=0 c=ERR m="ERR unknown command 'x'"`},
		{"-WRONGTYPE Operation\r\n", `- n=0 c=WRONGTYPE m="WRONGTYPE Operation"`},
		{"-MOVED 3999 127.0.0.1:6381\r\n", `- n=0 c=MOVED m="MOVED 3999 127.0.0.1:6381"`},
		{"-not an uppercase code\r\n", `- n=0 m="not an uppercase code"`},
		{":-42\r\n", `: n=0 v="-42"`},
		{"$5\r\nhello\r\n", `$ n=5 v="hello"`},
		{"$0\r\n\r\n", `$ n=0`},
		{"$-1\r\n", `$ n=0 null`},
		{"*-1\r\n", `* n=0 null`},
		{"*0\r\n", `* n=0`},
		{"*2\r\n$3\r\nfoo\r\n:1\r\n", `* n=2 h0=$"foo" h1=:"1"#1`},
		{"_\r\n", `_ n=0 null`},
		{",3.141\r\n", `, n=0 v="3.141"`},
		{",inf\r\n", `, n=0 v="inf"`},
		{"#t\r\n", `# n=0 v="t"`},
		{"(1234567999999999999999999999999999999\r\n", `( n=0 v="1234567999999999999999999999999999999"`},
		{"!21\r\nSYNTAX invalid syntax\r\n", `! n=21 c=SYNTAX m="SYNTAX invalid syntax"`},
		{"=15\r\ntxt:Some string\r\n", `= n=15 v="txt:Some string"`},
		{"%2\r\n+first\r\n:1\r\n+second\r\n:2\r\n", `% n=2 h0=+"first" h1=:"1"#1 h2=+""`},
		{"~2\r\n+a\r\n+b\r\n", `~ n=2 h0=+"a" h1=+"b"`},
		{">3\r\n$7\r\nmessage\r\n$2\r\nch\r\n$4\r\nbody\r\n", `> n=3 h0=$"message" h1=$"ch" h2=$""`},
		{">4\r\n$8\r\npmessage\r\n$3\r\nch*\r\n$2\r\nch\r\n$1\r\nx\r\n", `> n=4 h0=$"pmessage" h1=$"ch*" h2=$"ch"`},
		{"|1\r\n+key-popularity\r\n*2\r\n$7\r\nkey:123\r\n:90\r\n$2\r\nok\r\n", `$ n=2 attr v="ok"`},
		{"*2\r\n|1\r\n+a\r\n+b\r\n:1\r\n:2\r\n", `* n=2 h0=:"1"#1 h1=:"2"#2`},
		{"|0\r\n:7\r\n", `: n=0 attr v="7"`},
		{"+1791788445.996979 [0 127.0.0.1:59294] \"GET\" \"k\"\r\n", `+ n=0 monitor`},
	} {
		frames, losses := redisRead(redisResponseReader(), [][]byte{[]byte(tc.in)}, nil)
		if losses != 0 || len(frames) != 1 || frames[0] != tc.want {
			t.Errorf("%q: frames %q losses %d, want %q", tc.in, frames, losses, tc.want)
		}
		if got := redisFrameBytes(redisResponseReader(), tc.in); got != int64(len(tc.in)) {
			t.Errorf("%q: frame bytes %d, want %d", tc.in, got, len(tc.in))
		}
	}
}

// redisMixed는 모든 type byte와 attribute, push, 중첩 aggregate를 섞은 응답 흐름이다.
const redisMixed = "+OK\r\n-ERR bad\r\n:12\r\n$5\r\nhello\r\n$-1\r\n*-1\r\n*3\r\n:1\r\n*2\r\n+a\r\n$1\r\nb\r\n_\r\n" +
	",1.5\r\n#f\r\n(12345678901234567890\r\n!10\r\nERR blob x\r\n=8\r\ntxt:abcd\r\n%1\r\n$1\r\nk\r\n~1\r\n:2\r\n" +
	"|1\r\n+meta\r\n:3\r\n>2\r\n$10\r\ninvalidate\r\n*1\r\n$3\r\nkey\r\n*0\r\n$0\r\n\r\n"

func TestRedisReaderSplitAtEveryOffset(t *testing.T) {
	data := []byte(redisMixed)
	whole, losses := redisRead(redisResponseReader(), [][]byte{data}, nil)
	if losses != 0 || len(whole) != 16 {
		t.Fatalf("whole: %d frames, %d losses: %q", len(whole), losses, whole)
	}
	for cut := 1; cut < len(data); cut++ {
		got, losses := redisRead(redisResponseReader(), [][]byte{data[:cut], data[cut:]}, nil)
		if losses != 0 || strings.Join(got, "|") != strings.Join(whole, "|") {
			t.Fatalf("cut %d: %q losses %d", cut, got, losses)
		}
	}
	var single [][]byte
	for i := range data {
		single = append(single, data[i:i+1])
	}
	got, losses := redisRead(redisResponseReader(), single, nil)
	if losses != 0 || strings.Join(got, "|") != strings.Join(whole, "|") {
		t.Fatalf("byte by byte: %q losses %d", got, losses)
	}
}

func TestRedisReaderRejectsMalformedInput(t *testing.T) {
	deep := strings.Repeat("*1\r\n", redisDepthLimit+1) + ":1\r\n"
	for _, in := range []string{
		"$-5\r\nxx\r\n",
		"*-2\r\n",
		"%-1\r\n",
		"$abc\r\n",
		"$\r\n",
		"$+5\r\nhello\r\n",
		"*99999999999\r\n",
		"$536870913\r\n",
		"$99999999999999999999\r\n",
		"$1\r\nab\r\n",
		"$1\r\na\rx",
		"+OK\rX",
		"+OK\n",
		"?3\r\n",
		"$?\r\n;4\r\nabcd\r\n;0\r\n",
		"*?\r\n",
		"_x\r\n",
		"#tf\r\n",
		deep,
		"$" + strings.Repeat("1", redisLengthLine+1) + "\r\n",
	} {
		reader := redisResponseReader()
		frames, losses := redisRead(reader, [][]byte{[]byte(in)}, nil)
		if losses != 1 {
			t.Errorf("%q: frames %q losses %d, want a loss", in, frames, losses)
		}
		if !reader.idle() || reader.heldBytes() != 0 || len(reader.stack) != 0 {
			t.Errorf("%q: reader keeps state after loss", in)
		}
	}
	// 깊이 상한까지는 받는다.
	ok := strings.Repeat("*1\r\n", redisDepthLimit) + ":1\r\n"
	if frames, losses := redisRead(redisResponseReader(), [][]byte{[]byte(ok)}, nil); losses != 0 || len(frames) != 1 {
		t.Fatalf("depth %d: %q %d", redisDepthLimit, frames, losses)
	}
}

func TestRedisReaderSkipsBulkBeyondCapture(t *testing.T) {
	value := bytes.Repeat([]byte("v"), 200000)
	reply := append(append([]byte("$200000\r\n"), value...), "\r\n+PONG\r\n"...)
	big := reply[:200011]
	for _, capture := range []int{1024, 4095} {
		// 서버는 한 번에 쓰고, client는 16384 byte씩 읽는다. 어느 쪽이든 record의 앞 capture byte만 보인다.
		var records [][]byte
		var sizes []int
		for start := 0; start < len(big); start += 16384 {
			end := min(start+16384, len(big))
			records = append(records, big[start:min(end, start+capture)])
			sizes = append(sizes, end-start)
		}
		records = append(records, []byte("+PONG\r\n"))
		reader := &redisReader{}
		frames, losses := redisRead(reader, records, sizes)
		if losses != 0 || len(frames) != 2 || frames[0] != "$ n=200000" || frames[1] != "+ n=0" {
			t.Fatalf("cap %d: %q losses %d", capture, frames, losses)
		}
	}
	// header가 캡처한 byte 밖에 있으면 경계를 잃는다.
	items := []byte("*3\r\n" + strings.Repeat("$4\r\nitem\r\n", 3))
	reader := &redisReader{}
	if frames, losses := redisRead(reader, [][]byte{items[:12]}, []int{len(items)}); losses != 1 || len(frames) != 0 {
		t.Fatalf("hidden header: %q %d", frames, losses)
	}
	// 보이는 CRLF 자리가 다른 byte면 잃는다.
	if _, losses := redisRead(&redisReader{}, [][]byte{[]byte("$3\r\nabcXY")}, nil); losses != 1 {
		t.Fatalf("bad bulk terminator accepted")
	}
}

func redisRequestReader(showSecrets bool) *redisReader {
	return &redisReader{request: true, showSecrets: showSecrets}
}

func redisArgs(args ...string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "*%d\r\n", len(args))
	for _, arg := range args {
		fmt.Fprintf(&out, "$%d\r\n%s\r\n", len(arg), arg)
	}
	return out.String()
}

func TestRedisReaderDecodesCommands(t *testing.T) {
	for _, tc := range []struct {
		in          string
		showSecrets bool
		want        string
	}{
		{redisArgs("get", "user:1"), false, `* n=2 cmd="GET" key="user:1" args=1`},
		{redisArgs("SET", "k", "v", "EX", "10"), true, `* n=5 cmd="SET" key="k" args=4 preview=["k" "v" "EX" "10"]`},
		{redisArgs("SET", "k", "v"), false, `* n=3 cmd="SET" key="k" args=2`},
		{redisArgs("config", "get", "maxmemory"), true, `* n=3 cmd="CONFIG GET" key="" args=2 preview=["maxmemory"] sensitive`},
		{redisArgs("CONFIG", "SET", "masterauth", "edc-secret-master"), true, `* n=4 cmd="CONFIG SET" key="" args=3 sensitive`},
		{redisArgs("ACL", "SETUSER", "edc-user", "on", ">edc-secret-acl"), true, `* n=5 cmd="ACL SETUSER" key="" args=4 preview=["edc-user"] sensitive`},
		{redisArgs("AUTH", "edc-fixture-pass"), true, `* n=2 cmd="AUTH" key="" args=1 sensitive`},
		{redisArgs("AUTH", "default", "edc-fixture-pass"), true, `* n=3 cmd="AUTH" key="" args=2 sensitive`},
		{redisArgs("HELLO", "3", "AUTH", "default", "edc-fixture-pass", "SETNAME", "app"), true, `* n=7 cmd="HELLO" key="" args=6 preview=["3" "AUTH" "SETNAME" "app"] sensitive proto=3`},
		{redisArgs("HELLO", "2"), false, `* n=2 cmd="HELLO" key="" args=1 sensitive proto=2`},
		{redisArgs("MIGRATE", "127.0.0.1", "6390", "k", "0", "1000", "COPY", "AUTH2", "u", "edc-secret-migrate", "KEYS", "a"), true, `* n=12 cmd="MIGRATE" key="k" args=11 preview=["127.0.0.1" "6390" "k" "0" "1000" "COPY" "AUTH2"] sensitive`},
		{redisArgs("OBJECT", "ENCODING", "k"), false, `* n=3 cmd="OBJECT ENCODING" key="k" args=2`},
		{redisArgs("CLIENT", "REPLY", "skip"), false, `* n=3 cmd="CLIENT REPLY" key="" args=2 reply=SKIP`},
		{redisArgs("SUBSCRIBE", "a", "b"), false, `* n=3 cmd="SUBSCRIBE" key="" args=2`},
		{redisArgs("has space"), false, `* n=1 cmd="" key="" args=0`},
		{redisArgs(strings.Repeat("A", redisNameLimit+1)), false, `* n=1 cmd="" key="" args=0`},
		{"*0\r\n*-1\r\nPING\r\n", false, `i n=0 cmd="PING" key="" args=0`},
		{"set k  v\n", true, `i n=0 cmd="SET" key="k" args=2 preview=["k" "v"]`},
		{"\r\n", false, ``},
	} {
		frames, losses := redisRead(redisRequestReader(tc.showSecrets), [][]byte{[]byte(tc.in)}, nil)
		if got := strings.Join(frames, "|"); losses != 0 || got != tc.want {
			t.Errorf("%q: got %q losses %d, want %q", tc.in, got, losses, tc.want)
		}
		if got := redisFrameBytes(redisRequestReader(tc.showSecrets), tc.in); tc.want != "" && !strings.HasPrefix(tc.in, "*0") && got != int64(len(tc.in)) {
			t.Errorf("%q: frame bytes %d, want %d", tc.in, got, len(tc.in))
		}
	}
	// 요청 쪽은 bulk 문자열 배열과 inline만 받는다.
	for _, in := range []string{"*1\r\n:1\r\n", "*1\r\n*1\r\n$1\r\na\r\n", "*1\r\n$-1\r\n", "*2\r\n$3\r\nGET\r\n+k\r\n"} {
		if _, losses := redisRead(redisRequestReader(false), [][]byte{[]byte(in)}, nil); losses != 1 {
			t.Errorf("%q accepted as a request", in)
		}
	}
}

// redisStateHas는 reader가 쥔 byte와 decoder 상태에 needle이 있는지 본다. 용량 끝까지 본다.
func redisStateHas(reader *redisReader, needle string) bool {
	if bytes.Contains(reader.held[:cap(reader.held)], []byte(needle)) {
		return true
	}
	for _, text := range append(reader.command.preview[:cap(reader.command.preview)], reader.command.name, reader.command.key) {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func TestRedisReaderNeverHoldsSecretArguments(t *testing.T) {
	for _, tc := range []struct {
		name, in, secret string
	}{
		{"auth", redisArgs("AUTH", "edc-fixture-pass"), "edc-fixture"},
		{"auth2", redisArgs("AUTH", "default", "edc-fixture-pass"), "edc-fixture"},
		{"hello", redisArgs("HELLO", "3", "AUTH", "default", "edc-fixture-pass"), "edc-fixture"},
		{"config", redisArgs("CONFIG", "SET", "masterauth", "edc-secret-master"), "edc-secret"},
		{"acl", redisArgs("ACL", "SETUSER", "u", "on", ">edc-secret-acl"), "edc-secret"},
		{"migrate", redisArgs("MIGRATE", "h", "1", "k", "0", "1", "AUTH", "edc-secret-migrate"), "edc-secret"},
		{"inline", "AUTH edc-fixture-pass\r\n", "edc-fixture"},
	} {
		data := []byte(tc.in)
		for _, show := range []bool{false, true} {
			// 어느 자리에서 record가 나뉘어도 첫 record를 먹인 뒤 쥔 상태에 password가 없어야 한다.
			for cut := 1; cut < len(data); cut++ {
				reader := redisRequestReader(show)
				var frames []redisFrame
				reader.feed(data[:cut], cut, 1, func(f redisFrame) { frames = append(frames, f) })
				if redisStateHas(reader, tc.secret) {
					t.Fatalf("%s cut %d show %v: reader holds the secret", tc.name, cut, show)
				}
				reader.feed(data[cut:], len(data)-cut, 2, func(f redisFrame) { frames = append(frames, f) })
				if len(frames) != 1 || strings.Contains(fmt.Sprintf("%+v", *frames[0].command), tc.secret) || redisStateHas(reader, tc.secret) {
					t.Fatalf("%s cut %d show %v: %d frames, secret in %+v", tc.name, cut, show, len(frames), frames)
				}
			}
		}
	}
}

func TestRedisReaderLostRequestKeepsName(t *testing.T) {
	data := []byte(redisArgs("RPUSH", "list", strings.Repeat("x", 3000), "tail"))
	reader := redisRequestReader(false)
	var frames []string
	// 둘째 record의 앞 일부만 보여 마지막 인자의 header가 캡처 밖에 있다.
	var lostBytes int64
	collect := func(f redisFrame) { frames, lostBytes = append(frames, redisFrameText(f)), f.bytes }
	ok1 := reader.feed(data[:1024], 1024, 1, collect)
	ok2 := reader.feed(data[1024:1100], len(data)-1024, 2, collect)
	if !ok1 || ok2 || len(frames) != 1 || frames[0] != `* n=0 cmd="RPUSH" key="list" args=3 lost` || lostBytes != 3034 {
		t.Fatalf("frames %q ok %v %v", frames, ok1, ok2)
	}
	if !reader.idle() || reader.heldBytes() != 0 {
		t.Fatal("reader keeps state after loss")
	}
}

func TestRedisReaderPreviewLimits(t *testing.T) {
	value := strings.Repeat("v", 200)
	in := "$200\r\n" + value + "\r\n"
	frames, _ := redisRead(redisResponseReader(), [][]byte{[]byte(in)}, nil)
	if want := fmt.Sprintf("$ n=200 v=%q", value[:redisValueKeep]); len(frames) != 1 || frames[0] != want {
		t.Fatalf("preview: %q", frames)
	}
	// 남기라고 하지 않으면 값도 오류 message도 남기지 않는다.
	reader := &redisReader{keep: func(byte) redisKeep { return redisKeep{} }}
	frames, _ = redisRead(reader, [][]byte{[]byte(in + "-ERR edc-secret-value\r\n+OK\r\n")}, nil)
	if strings.Join(frames, "|") != `$ n=200|- n=0 c=ERR|+ n=0` {
		t.Fatalf("no keep: %q", frames)
	}
	if cap(reader.held) > redisValueKeep {
		t.Fatalf("held buffer grew to %d", cap(reader.held))
	}
}

func TestRedisReaderInlineNeedsTerminator(t *testing.T) {
	// 줄 끝이 캡처한 byte 밖이면 잃는다. 다음 record로 이어지는 줄은 받는다.
	if _, losses := redisRead(redisRequestReader(false), [][]byte{[]byte("GET k")}, []int{4000}); losses != 1 {
		t.Fatal("inline line past the captured bytes was accepted")
	}
	frames, losses := redisRead(redisRequestReader(false), [][]byte{[]byte("GE"), []byte("T k\r"), []byte("\n")}, nil)
	if losses != 0 || strings.Join(frames, "|") != `i n=0 cmd="GET" key="k" args=1` {
		t.Fatalf("split inline: %q %d", frames, losses)
	}
}
