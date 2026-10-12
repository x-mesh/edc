package edc

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
		{redisArgs("has space"), false, `* n=1 cmd="" key="" args=0 sensitive`},
		{redisArgs(strings.Repeat("A", redisNameLimit+1)), false, `* n=1 cmd="" key="" args=0 sensitive`},
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

func TestRedisReaderPreviewStopsAtHiddenBytes(t *testing.T) {
	in := redisArgs("GET", "edc-key-XXXXtail")
	cut := len("*2\r\n$3\r\nGET\r\n$16\r\nedc-key-")
	reader := redisRequestReader(false)
	var frames []string
	collect := func(f redisFrame) { frames = append(frames, redisFrameText(f)) }
	// 앞 record의 끝 4 byte(XXXX)는 캡처하지 못했다.
	reader.feed([]byte(in[:cut]), cut+4, 1, collect)
	reader.feed([]byte(in[cut+4:]), len(in)-cut-4, 2, collect)
	if len(frames) != 1 || frames[0] != `* n=2 cmd="GET" key="edc-key-" args=1` {
		t.Fatalf("frames %q", frames)
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

type redisFixtureRecord struct {
	run          int
	server, sent bool
	size         int
	payload      []byte
}

// loadRedisFixture는 T1이 VM의 Redis 7.0에서 잰 record를 읽는다. hex는 record의 앞 min(size, 4096) byte 전부다.
func loadRedisFixture(t testing.TB, name string) []redisFixtureRecord {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "redis", name+".records"))
	if err != nil {
		t.Fatal(err)
	}
	var records []redisFixtureRecord
	run := -1
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "# run "):
			run++
		case strings.HasPrefix(line, "#"), strings.TrimSpace(line) == "":
		default:
			fields := strings.Fields(line)
			if len(fields) != 4 {
				t.Fatalf("%s: bad record line %.60q", name, line)
			}
			size, err := strconv.Atoi(fields[2])
			if err != nil {
				t.Fatal(err)
			}
			payload, err := hex.DecodeString(fields[3])
			if err != nil || len(payload) != min(size, 4096) {
				t.Fatalf("%s: bad hex in %.60q", name, line)
			}
			records = append(records, redisFixtureRecord{run: run, server: fields[0] == "server", sent: fields[1] == "send", size: size, payload: payload})
		}
	}
	return records
}

func redisRecord(socket uint64, server, sent bool, at uint64, payload []byte, size int) mysqlRecord {
	record := mysqlRecord{bootTimeNS: at, pid: 7, process: "redis", socket: socket, server: server, sent: sent, size: size, payload: payload}
	if server {
		record.source, record.destination, record.localPort, record.peerPort = "127.0.0.1:6379", "127.0.0.1:40000", traceRedisDefaultPort, 40000
	} else {
		record.source, record.destination, record.localPort, record.peerPort = "127.0.0.1:40000", "127.0.0.1:6379", 40000, traceRedisDefaultPort
	}
	return record
}

// redisRequestCap과 응답 쪽 상한은 BPF가 record에서 넘기는 앞부분 길이다.
const redisRequestCap = 4095

// replayRedis는 한 쪽의 record를 run마다 다른 socket으로 한 tracker에 먹인다. 시각은 파일의 record 번호 x 1 ms다.
func replayRedis(records []redisFixtureRecord, server bool, responseCap int, showSecrets bool) (map[int][]redisTraced, *redisTracker) {
	tracker := newRedisTracker("", showSecrets)
	events := map[int][]redisTraced{}
	for i, record := range records {
		if record.server != server {
			continue
		}
		limit := responseCap
		if record.server != record.sent {
			limit = redisRequestCap
		}
		payload := bytes.Clone(record.payload[:min(len(record.payload), limit)])
		events[record.run] = append(events[record.run], tracker.events(redisRecord(uint64(record.run+1), server, record.sent, uint64(i+1)*1_000_000, payload, record.size), 0)...)
	}
	return events, tracker
}

// redisPairs는 명령 이름 순서와 응답이 답한 명령 이름 순서를 돌려준다. 응답은 모두 answered와 0 이상의 latency가 있어야 한다.
func redisPairs(t *testing.T, label string, events []redisTraced) (commands, answered []string) {
	t.Helper()
	for _, traced := range events {
		event, info := traced.event, traced.redis
		if event.Protocol != "redis" || info == nil {
			t.Fatalf("%s: bad event %+v", label, event)
		}
		switch event.Event {
		case redisEventCommand:
			if event.LatencyMS != nil || event.answered != 0 {
				t.Fatalf("%s: command with latency", label)
			}
			commands = append(commands, info.Command)
		case redisEventReply, redisEventError:
			if event.LatencyMS == nil || *event.LatencyMS < 0 || event.answered != 1 {
				t.Fatalf("%s: reply without latency: %+v %+v", label, event, info)
			}
			answered = append(answered, info.Command)
		case redisEventPush:
			if event.LatencyMS != nil || event.answered != 0 {
				t.Fatalf("%s: push with latency", label)
			}
		default:
			t.Fatalf("%s: unknown event %s", label, event.Event)
		}
	}
	return commands, answered
}

func redisAllAnswered(t *testing.T, label string, events []redisTraced) {
	t.Helper()
	commands, answered := redisPairs(t, label, events)
	if len(commands) == 0 || strings.Join(commands, ",") != strings.Join(answered, ",") {
		t.Fatalf("%s: commands %q answered %q", label, commands, answered)
	}
}

func redisSelect(events []redisTraced, name string) []*traceRedisEvent {
	var out []*traceRedisEvent
	for _, traced := range events {
		if traced.event.Event == name {
			out = append(out, traced.redis)
		}
	}
	return out
}

func TestRedisReplaysFixtures(t *testing.T) {
	for _, server := range []bool{true, false} {
		for _, responseCap := range []int{1024, 4095} {
			label := func(name string) string { return fmt.Sprintf("%s server=%v cap=%d", name, server, responseCap) }
			replay := func(name string) map[int][]redisTraced {
				events, _ := replayRedis(loadRedisFixture(t, name), server, responseCap, false)
				return events
			}

			pipeline := replay("pipeline")
			if len(pipeline) != 5 {
				t.Fatalf("%s: %d runs", label("pipeline"), len(pipeline))
			}
			for run, events := range pipeline {
				redisAllAnswered(t, label(fmt.Sprintf("pipeline run %d", run)), events)
			}
			if commands, _ := redisPairs(t, label("pipeline"), pipeline[1]); len(commands) != 65 || commands[1] != "SET" {
				t.Fatalf("%s: run 1 commands %q", label("pipeline"), commands)
			}

			pubsub := replay("pubsub")
			redisAllAnswered(t, label("pubsub"), pubsub[0])
			redisAllAnswered(t, label("pubsub publisher"), pubsub[1])
			var pushes []string
			for _, info := range redisSelect(pubsub[0], redisEventPush) {
				pushes = append(pushes, info.PushKind+":"+info.Channel)
			}
			if strings.Join(pushes, " ") != "message:edc-ch1 pmessage:edc-ch1 message:edc-ch2 pmessage:edc-ch2" {
				t.Fatalf("%s: pushes %q", label("pubsub"), pushes)
			}

			multi := replay("multi")
			redisAllAnswered(t, label("multi"), multi[0])
			var replies []string
			for _, info := range append(redisSelect(multi[0], redisEventReply), redisSelect(multi[0], redisEventError)...) {
				if info.Command == "EXEC" {
					text := info.Reply
					if info.Elements != nil {
						text += fmt.Sprint(*info.Elements)
					}
					replies = append(replies, text+info.ErrorClass)
				}
			}
			queued := 0
			for _, info := range redisSelect(multi[0], redisEventReply) {
				if info.Reply == "queued" {
					queued++
				}
			}
			if strings.Join(replies, " ") != "array2 null array2 errorEXECABORT" || queued != 7 {
				t.Fatalf("%s: EXEC replies %q queued %d", label("multi"), replies, queued)
			}

			large := replay("large_get")
			redisAllAnswered(t, label("large_get set"), large[0])
			redisAllAnswered(t, label("large_get get"), large[1])
			get := redisSelect(large[1], redisEventReply)
			if len(get) != 2 || get[1].Command != "GET" || get[1].BulkLength == nil || *get[1].BulkLength != 200000 {
				t.Fatalf("%s: GET reply %+v", label("large_get"), get)
			}

			resp3 := replay("resp3")
			redisAllAnswered(t, label("resp3"), resp3[0])
			kinds := map[string]bool{}
			for _, info := range redisSelect(resp3[0], redisEventReply) {
				kinds[info.Reply] = true
			}
			for _, kind := range []string{"simple", "integer", "bulk", "array", "map", "set", "null", "double", "boolean", "bignum", "verbatim", "push"} {
				if !kinds[kind] {
					t.Fatalf("%s: no %s reply in %v", label("resp3"), kind, kinds)
				}
			}
			pushes = nil
			for _, info := range redisSelect(resp3[0], redisEventPush) {
				pushes = append(pushes, info.PushKind)
			}
			if strings.Join(pushes, " ") != "server-cpu-usage invalidate message" {
				t.Fatalf("%s: pushes %q", label("resp3"), pushes)
			}
			if last := redisSelect(resp3[0], redisEventReply); last[len(last)-1].RESP != 3 {
				t.Fatalf("%s: HELLO 3 did not switch the protocol", label("resp3"))
			}

			monitor := replay("monitor")
			redisAllAnswered(t, label("monitor"), monitor[0])
			if len(monitor[0]) != 12 {
				t.Fatalf("%s: %d events, want 6 commands and 6 replies", label("monitor"), len(monitor[0]))
			}

			errors := replay("errors")
			redisAllAnswered(t, label("errors"), errors[0])
			var classes []string
			for _, info := range redisSelect(errors[0], redisEventError) {
				classes = append(classes, info.ErrorClass)
			}
			if strings.Join(classes, " ") != "NOAUTH NOAUTH NOAUTH WRONGPASS WRONGTYPE ERR NOSCRIPT" {
				t.Fatalf("%s: classes %q", label("errors"), classes)
			}

			secrets := replay("secrets")
			redisAllAnswered(t, label("secrets"), secrets[0])

			inline := replay("inline")
			redisAllAnswered(t, label("inline"), inline[0])
			for _, info := range redisSelect(inline[0], redisEventCommand) {
				if !info.Inline {
					t.Fatalf("%s: command %q not inline", label("inline"), info.Command)
				}
			}

			lrange := replay("lrange_big")
			commands, answered := redisPairs(t, label("lrange_big"), lrange[0])
			if strings.Join(commands, ",") != "AUTH,RPUSH,LRANGE,PING" || strings.Join(answered, ",") != "AUTH,PING" {
				t.Fatalf("%s: commands %q answered %q", label("lrange_big"), commands, answered)
			}
			if rpush := redisSelect(lrange[0], redisEventCommand)[1]; !rpush.Truncated || rpush.Key != "edc-list" {
				t.Fatalf("%s: RPUSH %+v", label("lrange_big"), rpush)
			}
		}
	}
}

// redisStep은 표 test의 record 하나다. request가 참이면 client가 보낸 요청이다.
type redisStep struct {
	request bool
	data    string
	size    int
}

func redisClient(tracker *redisTracker, socket uint64, steps []redisStep) []redisTraced {
	var out []redisTraced
	for i, step := range steps {
		size := step.size
		if size == 0 {
			size = len(step.data)
		}
		out = append(out, tracker.events(redisRecord(socket, false, step.request, uint64(i+1)*1_000_000, []byte(step.data), size), 0)...)
	}
	return out
}

// redisTokens는 event를 짧게 쓴다. C는 명령, R은 응답(명령/종류), E는 오류(명령/class), P는 push다.
func redisTokens(events []redisTraced) string {
	var tokens []string
	for _, traced := range events {
		info := traced.redis
		switch traced.event.Event {
		case redisEventCommand:
			tokens = append(tokens, "C:"+info.Command)
		case redisEventReply:
			tokens = append(tokens, fmt.Sprintf("R:%s/%s", info.Command, info.Reply))
		case redisEventError:
			tokens = append(tokens, fmt.Sprintf("E:%s/%s", info.Command, info.ErrorClass))
		case redisEventPush:
			tokens = append(tokens, "P:"+info.PushKind)
		}
	}
	return strings.Join(tokens, " ")
}

func redisStart(steps ...redisStep) []redisStep {
	// 새 socket은 응답 쪽 경계를 모르므로 첫 명령의 응답으로 맞춘다.
	return append([]redisStep{{true, redisArgs("PING"), 0}, {false, "+PONG\r\n", 0}}, steps...)
}

func TestRedisTrackerPairsTableInputs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []redisStep
		want  string
	}{
		{"error classes", redisStart(
			redisStep{true, redisArgs("GET", "a") + redisArgs("GET", "b") + redisArgs("GET", "c") + redisArgs("GET", "d") + redisArgs("GET", "e"), 0},
			redisStep{false, "-MOVED 3999 127.0.0.1:6381\r\n-ASK 3999 127.0.0.1:6381\r\n-WRONGTYPE Operation\r\n-NOAUTH Authentication required.\r\n-oops lowercase\r\n", 0},
		), "C:PING R:PING/simple C:GET C:GET C:GET C:GET C:GET E:GET/MOVED E:GET/ASK E:GET/WRONGTYPE E:GET/NOAUTH E:GET/"},
		{"client reply off", redisStart(
			redisStep{true, redisArgs("CLIENT", "REPLY", "OFF"), 0},
			redisStep{true, redisArgs("SET", "a", "1"), 0},
			redisStep{true, redisArgs("CLIENT", "REPLY", "ON"), 0},
			redisStep{false, "+OK\r\n", 0},
			redisStep{true, redisArgs("GET", "a"), 0},
			redisStep{false, "$1\r\n1\r\n", 0},
		), "C:PING R:PING/simple C:CLIENT REPLY C:SET C:CLIENT REPLY R:CLIENT REPLY/simple C:GET R:GET/bulk"},
		{"client reply skip", redisStart(
			redisStep{true, redisArgs("CLIENT", "REPLY", "SKIP") + redisArgs("SET", "a", "1") + redisArgs("GET", "a"), 0},
			redisStep{false, "$1\r\n1\r\n", 0},
		), "C:PING R:PING/simple C:CLIENT REPLY C:SET C:GET R:GET/bulk"},
		{"hello switch and noproto", redisStart(
			redisStep{true, redisArgs("HELLO", "3"), 0},
			redisStep{false, "%1\r\n$5\r\nproto\r\n:3\r\n", 0},
			redisStep{true, redisArgs("HELLO", "4"), 0},
			redisStep{false, "-NOPROTO unsupported protocol version\r\n", 0},
			redisStep{true, redisArgs("SUBSCRIBE", "a"), 0},
			redisStep{false, ">3\r\n$9\r\nsubscribe\r\n$1\r\na\r\n:1\r\n", 0},
			redisStep{true, redisArgs("LRANGE", "l", "0", "-1"), 0},
			// RESP3에서는 "message"로 시작하는 배열도 응답이다.
			redisStep{false, "*2\r\n$7\r\nmessage\r\n$1\r\nx\r\n", 0},
			redisStep{true, redisArgs("RESET"), 0},
			redisStep{false, "+RESET\r\n", 0},
		), "C:PING R:PING/simple C:HELLO R:HELLO/map C:HELLO E:HELLO/NOPROTO C:SUBSCRIBE R:SUBSCRIBE/push C:LRANGE R:LRANGE/array C:RESET R:RESET/simple"},
		{"resp2 subscribe confirmations", redisStart(
			redisStep{true, redisArgs("SUBSCRIBE", "a", "b") + redisArgs("SUBSCRIBE", "c"), 0},
			redisStep{false, "*3\r\n$9\r\nsubscribe\r\n$1\r\na\r\n:1\r\n*3\r\n$9\r\nsubscribe\r\n$1\r\nb\r\n:2\r\n*3\r\n$9\r\nsubscribe\r\n$1\r\nc\r\n:3\r\n", 0},
			redisStep{false, "*3\r\n$7\r\nmessage\r\n$1\r\na\r\n$4\r\nbody\r\n", 0},
			redisStep{true, redisArgs("PING"), 0},
			redisStep{false, "*2\r\n$4\r\npong\r\n$0\r\n\r\n", 0},
			redisStep{true, redisArgs("UNSUBSCRIBE"), 0},
			redisStep{false, "*3\r\n$11\r\nunsubscribe\r\n$1\r\na\r\n:2\r\n*3\r\n$11\r\nunsubscribe\r\n$1\r\nb\r\n:1\r\n*3\r\n$11\r\nunsubscribe\r\n$1\r\nc\r\n:0\r\n", 0},
			redisStep{true, redisArgs("LRANGE", "l", "0", "-1"), 0},
			// 구독이 끝나면 "message"로 시작하는 배열도 LRANGE의 응답이다.
			redisStep{false, "*2\r\n$7\r\nmessage\r\n$1\r\nx\r\n", 0},
		), "C:PING R:PING/simple C:SUBSCRIBE C:SUBSCRIBE R:SUBSCRIBE/array R:SUBSCRIBE/array P:message C:PING R:PING/array C:UNSUBSCRIBE R:UNSUBSCRIBE/array C:LRANGE R:LRANGE/array"},
		{"monitor lines", redisStart(
			redisStep{true, redisArgs("MONITOR"), 0},
			redisStep{false, "+OK\r\n+1791788445.996979 [0 127.0.0.1:1] \"SET\" \"k\" \"v\"\r\n", 0},
			redisStep{false, "+1791788445.996980 [0 127.0.0.1:1] \"GET\" \"k\"\r\n", 0},
			redisStep{true, redisArgs("RESET"), 0},
			redisStep{false, "+1791788445.996981 [0 127.0.0.1:1] \"GET\" \"k\"\r\n+RESET\r\n", 0},
		), "C:PING R:PING/simple C:MONITOR R:MONITOR/simple C:RESET R:RESET/simple"},
		{"multi", redisStart(
			redisStep{true, redisArgs("MULTI") + redisArgs("INCR", "a") + redisArgs("EXEC"), 0},
			redisStep{false, "+OK\r\n+QUEUED\r\n*1\r\n:1\r\n", 0},
		), "C:PING R:PING/simple C:MULTI C:INCR C:EXEC R:MULTI/simple R:INCR/queued R:EXEC/array"},
		{"mid-connection start", []redisStep{
			{false, "+OK\r\n", 0},
			{true, "ue\r\n" + redisArgs("GET", "x"), 0},
			{false, ":1\r\n", 0},
			{true, redisArgs("GET", "a"), 0},
			{false, "$1\r\nb\r\n", 0},
		}, "C:GET R:GET/bulk"},
		{"dropped response record", redisStart(
			redisStep{true, redisArgs("GET", "a") + redisArgs("GET", "b") + redisArgs("GET", "c"), 0},
			redisStep{false, "$1\r\na\r\n", 0},
			// "$1\r\nb\r\n$1\r" record를 잃었다.
			redisStep{false, "\nc\r\n", 0},
			redisStep{true, redisArgs("PING"), 0},
			redisStep{false, "+PONG\r\n", 0},
		), "C:PING R:PING/simple C:GET C:GET C:GET R:GET/bulk C:PING R:PING/simple"},
		{"header past the capture", redisStart(
			redisStep{true, redisArgs("LRANGE", "l", "0", "-1"), 0},
			redisStep{false, "*3\r\n$1\r\na\r\n", 100},
			// 같은 응답의 나머지가 frame처럼 보여도 대기 명령보다 frame이 많으면 맞추지 않는다.
			redisStep{false, "$1\r\nb\r\n$1\r\nc\r\n", 0},
			redisStep{true, redisArgs("PING"), 0},
			redisStep{false, "+PONG\r\n", 0},
		), "C:PING R:PING/simple C:LRANGE C:PING R:PING/simple"},
	} {
		tracker := newRedisTracker("", false)
		if got := redisTokens(redisClient(tracker, 1, tc.steps)); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
}

func TestRedisTrackerSideAndLatency(t *testing.T) {
	tracker := newRedisTracker(traceServerSide, false)
	if events := redisClient(tracker, 1, redisStart()); len(events) != 0 {
		t.Fatalf("client records passed the server side filter: %d", len(events))
	}
	tracker = newRedisTracker("", false)
	request := redisRecord(5, true, false, 10_000_000, []byte(redisArgs("GET", "k")), 0)
	request.size = len(request.payload)
	events := tracker.events(request, 100)
	reply := redisRecord(5, true, true, 13_500_000, []byte("$1\r\nv\r\n"), 7)
	events = append(events, tracker.events(reply, 100)...)
	if len(events) != 2 || events[1].event.LatencyMS == nil || *events[1].event.LatencyMS != 3.5 || events[1].event.Side != traceServerSide ||
		events[1].event.TimestampNS != 13_500_100 || events[1].event.Bytes != 7 || events[1].redis.Key != "k" || events[1].redis.BulkLength == nil {
		t.Fatalf("events %+v", events)
	}
}

func TestRedisTrackerLimits(t *testing.T) {
	tracker := newRedisTracker("", false)
	ping := []byte(redisArgs("PING"))
	for socket := uint64(1); socket <= redisSocketLimit; socket++ {
		tracker.events(redisRecord(socket, false, true, 1, ping, len(ping)), 0)
	}
	if len(tracker.conns) != redisSocketLimit || tracker.pending != redisSocketLimit {
		t.Fatalf("conns %d pending %d", len(tracker.conns), tracker.pending)
	}
	tracker.events(redisRecord(redisSocketLimit+1, false, true, 1, ping, len(ping)), 0)
	if len(tracker.conns) != 1 || tracker.pending != 1 {
		t.Fatalf("after the limit: conns %d pending %d", len(tracker.conns), tracker.pending)
	}
	tracker.forgetSocket(redisSocketLimit + 1)
	if len(tracker.conns) != 0 || tracker.pending != 0 {
		t.Fatalf("after forget: conns %d pending %d", len(tracker.conns), tracker.pending)
	}

	// 한 연결의 대기 명령이 상한을 넘으면 오래된 것만 버리지 않고 모두 응답 없음으로 버린다.
	tracker = newRedisTracker("", false)
	redisClient(tracker, 1, redisStart())
	many := []byte(strings.Repeat(redisArgs("PING"), redisPendingLimit+1))
	tracker.events(redisRecord(1, false, true, 5_000_000, many, len(many)), 0)
	conn := tracker.conns[1]
	if len(conn.pending) != 0 || tracker.pending != 0 || conn.responseSynced {
		t.Fatalf("pending %d total %d synced %v", len(conn.pending), tracker.pending, conn.responseSynced)
	}
	if events := tracker.events(redisRecord(1, false, false, 6_000_000, []byte("+PONG\r\n"), 7), 0); len(events) != 0 {
		t.Fatalf("reply after overflow paired: %s", redisTokens(events))
	}
}

// redisPlanted는 fixture와 표 입력에 심은 가짜 비밀이다. 6fcd8525…는 ACL이 돌려주는 edc-secret-acl의 SHA-256이다.
var redisPlanted = []string{"edc-fixture-pass", "edc-wrong-pass", "edc-secret-master", "edc-secret-acl", "edc-secret-migrate", "6fcd85253a6002c4"}

// redisWalk는 v에서 닿는 모든 문자열과 byte slice를 용량 끝까지 본다. 비공개 필드, pointer, map, interface도 따라간다.
func redisWalk(v reflect.Value, seen map[uintptr]bool, visit func(string)) {
	switch v.Kind() {
	case reflect.String:
		visit(v.String())
	case reflect.Pointer:
		if v.IsNil() || seen[v.Pointer()] {
			return
		}
		seen[v.Pointer()] = true
		redisWalk(v.Elem(), seen, visit)
	case reflect.Interface:
		if !v.IsNil() {
			redisWalk(v.Elem(), seen, visit)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			redisWalk(v.Field(i), seen, visit)
		}
	case reflect.Array:
		for i := range v.Len() {
			redisWalk(v.Index(i), seen, visit)
		}
	case reflect.Slice:
		if v.IsNil() {
			return
		}
		full := v.Slice(0, v.Cap())
		if v.Type().Elem().Kind() == reflect.Uint8 {
			visit(string(full.Bytes()))
			return
		}
		for i := range full.Len() {
			redisWalk(full.Index(i), seen, visit)
		}
	case reflect.Map:
		for iter := v.MapRange(); iter.Next(); {
			redisWalk(iter.Key(), seen, visit)
			redisWalk(iter.Value(), seen, visit)
		}
	}
}

// redisCheckSecrets는 event JSON과 tracker 상태에 심은 비밀이 없는지 본다. value가 보이는지 돌려준다.
func redisCheckSecrets(t *testing.T, label string, events []redisTraced, tracker *redisTracker, value string) bool {
	t.Helper()
	var text strings.Builder
	for _, traced := range events {
		for _, part := range []any{traced.event, traced.redis} {
			raw, err := json.Marshal(part)
			if err != nil {
				t.Fatal(err)
			}
			text.Write(raw)
		}
	}
	for _, secret := range redisPlanted {
		if strings.Contains(text.String(), secret) {
			t.Fatalf("%s: event JSON contains %s", label, secret)
		}
	}
	redisWalk(reflect.ValueOf(tracker), map[uintptr]bool{}, func(s string) {
		for _, secret := range redisPlanted {
			if strings.Contains(s, secret) {
				t.Fatalf("%s: tracker state contains %s", label, secret)
			}
		}
	})
	return strings.Contains(text.String(), value)
}

func TestRedisSecretsNeverKept(t *testing.T) {
	// walk가 쥔 byte와 대기 명령까지 닿는지 먼저 확인한다.
	probe := newRedisTracker("", false)
	redisClient(probe, 1, []redisStep{{true, redisArgs("GET", "walk-key") + "*2\r\n$3\r\nGET\r\n$10\r\nwalk-pr", 0}})
	found := map[string]bool{}
	redisWalk(reflect.ValueOf(probe), map[uintptr]bool{}, func(s string) {
		found["walk-key"] = found["walk-key"] || strings.Contains(s, "walk-key")
		found["walk-pr"] = found["walk-pr"] || strings.Contains(s, "walk-pr")
	})
	if !found["walk-key"] || !found["walk-pr"] {
		t.Fatalf("walk misses tracker state: %v", found)
	}
	for _, show := range []bool{false, true} {
		for _, server := range []bool{true, false} {
			for _, responseCap := range []int{1024, 4095} {
				label := fmt.Sprintf("show=%v server=%v cap=%d", show, server, responseCap)
				for _, name := range []string{"secrets", "errors", "monitor", "inline", "pipeline", "resp3"} {
					runs, tracker := replayRedis(loadRedisFixture(t, name), server, responseCap, show)
					var events []redisTraced
					for run := range len(runs) {
						events = append(events, runs[run]...)
					}
					seen := redisCheckSecrets(t, label+" "+name, events, tracker, "edc-secret-value")
					if name == "secrets" && seen != show {
						t.Fatalf("%s: edc-secret-value visible %v", label, seen)
					}
				}
			}
		}
	}
	// 표 입력은 record를 모든 자리에서 나누고, 매 record 뒤 tracker 상태를 본다.
	steps := redisStart(
		redisStep{true, redisArgs("AUTH", "default", "edc-fixture-pass"), 0},
		redisStep{false, "-WRONGPASS invalid username-password pair 'edc-fixture-pass'\r\n", 0},
		redisStep{true, "AUTH edc-wrong-pass\r\n", 0},
		redisStep{false, "+OK\r\n", 0},
		redisStep{true, redisArgs("CONFIG", "GET", "masterauth"), 0},
		redisStep{false, "*2\r\n$10\r\nmasterauth\r\n$17\r\nedc-secret-master\r\n", 0},
		redisStep{true, redisArgs("ACL", "GETUSER", "u"), 0},
		redisStep{false, "*2\r\n$9\r\npasswords\r\n*1\r\n$64\r\n6fcd85253a6002c4d4495e59b55843138d8ef21148dc4f26b5841983909ae0b4\r\n", 0},
		redisStep{true, redisArgs("MONITOR"), 0},
		redisStep{false, "+OK\r\n+1791788445.996979 [0 127.0.0.1:1] \"CONFIG\" \"SET\" \"masterauth\" \"edc-secret-master\"\r\n", 0},
		redisStep{true, redisArgs("RESET"), 0},
		redisStep{false, "+RESET\r\n", 0},
		redisStep{true, redisArgs("SET", "k", "edc-secret-value"), 0},
		redisStep{false, "+OK\r\n", 0},
	)
	for _, show := range []bool{false, true} {
		for step := range steps {
			data := steps[step].data
			for cut := 1; cut < len(data); cut++ {
				split := append(append(append([]redisStep(nil), steps[:step]...), redisStep{steps[step].request, data[:cut], 0}, redisStep{steps[step].request, data[cut:], 0}), steps[step+1:]...)
				tracker := newRedisTracker("", show)
				var events []redisTraced
				for i := range split {
					events = append(events, redisClient(tracker, 1, split[i:i+1])...)
					redisCheckSecrets(t, fmt.Sprintf("show=%v step %d cut %d", show, step, cut), events, tracker, "")
				}
			}
		}
	}
}

// redisFuzzSecret는 fuzz 입력 앞에 붙이는 AUTH의 password다. seed에는 없다.
const redisFuzzSecret = "edc-fuzz-secret-42"

// redisFuzzCut은 fuzz의 record 하나를 6 byte로 적는다. flags의 bit 0은 응답 방향, bit 1은 이 record 앞에서 socket을 잊으라는
// 뜻이다. n은 data에서 가져올 byte 수, hidden은 그 뒤에 캡처하지 못한 byte 수다.
func redisFuzzCut(response, forget bool, n, hidden int) []byte {
	flags := byte(0)
	if response {
		flags |= 1
	}
	if forget {
		flags |= 2
	}
	return []byte{flags, byte(n), byte(n >> 8), byte(hidden), byte(hidden >> 8), byte(hidden >> 16)}
}

// redisFuzzSeed는 fixture의 한 run과 쪽을 fuzz 입력으로 바꾼다. 큰 입력은 새 입력을 줄이는 데 오래 걸려 fuzz가 멈춘 것처럼
// 되므로 record마다 앞 redisFuzzSeedKeep byte만 남기고 나머지는 캡처하지 못한 byte로 둔다.
const (
	redisFuzzSeedKeep    = 512
	redisFuzzSeedRecords = 48
)

func redisFuzzSeed(records []redisFixtureRecord, run int, server bool) (data, cuts []byte) {
	for _, record := range records {
		if record.run != run || record.server != server || len(cuts) >= 6*redisFuzzSeedRecords {
			continue
		}
		payload := record.payload[:min(len(record.payload), redisFuzzSeedKeep)]
		data = append(data, payload...)
		cuts = append(cuts, redisFuzzCut(record.server == record.sent, false, len(payload), record.size-len(payload))...)
	}
	return data, cuts
}

func FuzzRedisTracker(f *testing.F) {
	for _, name := range []string{"pipeline", "pubsub", "multi", "large_get", "resp3", "monitor", "secrets", "errors", "inline", "lrange_big"} {
		records := loadRedisFixture(f, name)
		runs := 0
		for _, record := range records {
			runs = max(runs, record.run+1)
		}
		for run := range runs {
			for _, server := range []bool{true, false} {
				data, cuts := redisFuzzSeed(records, run, server)
				f.Add(data, cuts, uint16(1023), server)
				f.Add(data, cuts, uint16(4094), server)
			}
		}
	}
	table := []struct {
		request, response string
	}{
		{redisArgs("GET", "a") + redisArgs("SUBSCRIBE", "c"), redisMixed},
		{"PING\r\nSET k v\nAUTH x\r\n", "+PONG\r\n+OK\r\n-WRONGPASS bad\r\n"},
		{redisArgs("MONITOR") + redisArgs("RESET"), "+OK\r\n+1791788445.996979 [0 127.0.0.1:1] \"GET\" \"k\"\r\n+RESET\r\n"},
		{redisArgs("CLIENT", "REPLY", "SKIP") + redisArgs("HELLO", "3") + redisArgs("GET", "k"), "%1\r\n$5\r\nproto\r\n:3\r\n_\r\n"},
		{"*99999999999\r\n$-5\r\n", "$-5\r\n*99999999999\r\n" + strings.Repeat("*1\r\n", 40)},
	}
	for _, tc := range table {
		data := []byte(tc.request + tc.response)
		cuts := append(redisFuzzCut(false, false, len(tc.request), 0), redisFuzzCut(true, false, len(tc.response), 0)...)
		f.Add(data, cuts, uint16(4095), false)
		f.Add(data, append(cuts, redisFuzzCut(true, true, 3, 100)...), uint16(7), true)
	}
	f.Fuzz(func(t *testing.T, data, cuts []byte, capture uint16, server bool) {
		limit := int(capture)%4096 + 1
		tracker := newRedisTracker("", false)
		var events []redisTraced
		at := uint64(0)
		feed := func(response bool, payload []byte, size int) {
			at += 1_000_000
			// 서버는 요청을 받고(sent=false) 응답을 보낸다. client는 반대다.
			sent := response == server
			events = append(events, tracker.events(redisRecord(1, server, sent, at, payload, size), 0)...)
		}
		auth := []byte(redisArgs("AUTH", redisFuzzSecret))
		split := len(cuts) % len(auth)
		feed(false, auth[:split], split)
		feed(false, auth[split:], len(auth)-split)
		feed(true, []byte("+OK\r\n"), 5)
		redisFuzzRecords(data, cuts, limit, func() { tracker.forgetSocket(1) }, feed)
		for _, traced := range events {
			if latency := traced.event.LatencyMS; latency != nil && *latency < 0 {
				t.Fatalf("negative latency %v", *latency)
			}
		}
		for _, conn := range tracker.conns {
			if len(conn.pending) > redisPendingLimit {
				t.Fatalf("pending %d", len(conn.pending))
			}
			for _, reader := range []*redisReader{&conn.request, &conn.response} {
				if len(reader.held) > redisLineKeep || cap(reader.held) > redisLineKeep || len(reader.stack) > redisDepthLimit {
					t.Fatalf("reader holds %d bytes (cap %d), depth %d", len(reader.held), cap(reader.held), len(reader.stack))
				}
			}
		}
		if bytes.Contains(data, []byte(redisFuzzSecret)) {
			return
		}
		for _, traced := range events {
			raw, err := json.Marshal(traced.redis)
			if err != nil || bytes.Contains(raw, []byte(redisFuzzSecret)) {
				t.Fatalf("event %s carries the AUTH password (%v)", raw, err)
			}
		}
		redisWalk(reflect.ValueOf(tracker), map[uintptr]bool{}, func(s string) {
			if strings.Contains(s, redisFuzzSecret) {
				t.Fatal("tracker state keeps the AUTH password")
			}
		})
	})
}

// redisPreviewOf는 show-secrets reader로 요청 record들을 읽고 frame 문자열을 돌려준다.
func redisPreviewOf(records []string, sizes []int) string {
	var data [][]byte
	for _, record := range records {
		data = append(data, []byte(record))
	}
	frames, _ := redisRead(redisRequestReader(true), data, sizes)
	return strings.Join(frames, "|")
}

func TestRedisReaderSecretRangeFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		records []string
		sizes   []int
	}{
		{"inline quoted CONFIG SET", []string{"CONFIG \"SET\" requirepass edc-pw\r\n"}, nil},
		{"inline quoted HELLO AUTH", []string{"HELLO 3 \"AUTH\" u edc-pw\r\n"}, nil},
		{"inline quoted ACL SETUSER", []string{"ACL 'SETUSER' u >edc-pw\r\n"}, nil},
		{"inline quoted MIGRATE AUTH", []string{"MIGRATE h 1 k 0 1 \"AUTH\" edc-pw\r\n"}, nil},
		{"inline quoted HELLO user", []string{"HELLO 3 AUTH \"us er\" edc-pw\r\n"}, nil},
		{"inline escaped CONFIG SET", []string{"CONFIG \"S\\x45T\" requirepass edc-pw\r\n"}, nil},
		// subcommand의 일부를 캡처하지 못해 읽을 수 없다.
		{"hidden CONFIG subcommand", []string{"*4\r\n$6\r\nCONFIG\r\n$3\r\nS", "\r\n$11\r\nrequirepass\r\n$6\r\nedc-pw\r\n"}, []int{len("*4\r\n$6\r\nCONFIG\r\n$3\r\nS") + 2, 0}},
		{"hidden HELLO token", []string{"*5\r\n$5\r\nHELLO\r\n$1\r\n3\r\n$4\r\nAU", "\r\n$1\r\nu\r\n$6\r\nedc-pw\r\n"}, []int{len("*5\r\n$5\r\nHELLO\r\n$1\r\n3\r\n$4\r\nAU") + 2, 0}},
		{"unknown MIGRATE option", []string{redisArgs("MIGRATE", "h", "1", "k", "0", "1", "AUTX", "edc-pw")}, nil},
		{"SENTINEL SET", []string{redisArgs("SENTINEL", "SET", "m", "auth-pass", "edc-pw")}, nil},
		{"SENTINEL CONFIG SET", []string{redisArgs("SENTINEL", "CONFIG", "SET", "sentinel-pass", "edc-pw")}, nil},
	} {
		got := redisPreviewOf(tc.records, tc.sizes)
		if got == "" || strings.Contains(got, "edc-pw") || !strings.Contains(got, "sensitive") {
			t.Errorf("%s: %s", tc.name, got)
		}
	}
	// 따옴표와 escape는 sdssplitargs처럼 푼다. 닫히지 않은 따옴표와 닫는 따옴표 뒤의 글자는 protocol 오류다.
	if got := redisPreviewOf([]string{"SET \"a\\x41\\n\" 'b\\'c' \"\"\r\n"}, nil); got != `i n=0 cmd="SET" key="aA\n" args=3 preview=["aA\n" "b'c"]` {
		t.Errorf("quoted inline: %s", got)
	}
	for _, in := range []string{"SET \"a b\r\n", "SET 'a'b\r\n"} {
		if _, losses := redisRead(redisRequestReader(true), [][]byte{[]byte(in)}, nil); losses != 1 {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestRedisTrackerStopsAtReplication(t *testing.T) {
	tracker := newRedisTracker("", false)
	got := redisTokens(redisClient(tracker, 1, redisStart(
		redisStep{true, redisArgs("PSYNC", "?", "-1"), 0},
		redisStep{false, "+FULLRESYNC 8de1787ba490483314a4d30f1c628bc5025eb761 0\r\n$5\r\nREDIS", 0},
		redisStep{true, redisArgs("REPLCONF", "ACK", "0"), 0},
		redisStep{false, redisArgs("REPLCONF", "GETACK", "*"), 0},
		redisStep{true, redisArgs("REPLCONF", "ACK", "31"), 0},
		redisStep{false, redisArgs("SELECT", "0") + redisArgs("SET", "k", "v"), 0},
	)))
	if got != "C:PING R:PING/simple C:PSYNC R:PSYNC/simple C:REPLCONF ACK C:REPLCONF ACK" {
		t.Fatalf("got %s", got)
	}
}

func TestRedisTrackerWaitsAfterRequestLoss(t *testing.T) {
	// 4096 byte보다 큰 pipeline record의 뒷부분을 캡처하지 못했다. 그 사이 client가 GET new를 보낸 뒤 옛 응답이 온다.
	pipeline := strings.Repeat(redisArgs("GET", "old"), 300)
	tracker := newRedisTracker("", false)
	got := redisTokens(redisClient(tracker, 1, redisStart(
		redisStep{true, pipeline[:4095], len(pipeline)},
		redisStep{true, redisArgs("GET", "new"), 0},
		redisStep{false, "$3\r\nold\r\n", 0},
		redisStep{false, "$3\r\nnew\r\n", 0},
		redisStep{true, redisArgs("PING"), 0},
		redisStep{false, "+PONG\r\n", 0},
	)))
	if !strings.HasSuffix(got, "C:GET C:PING R:PING/simple") || strings.Contains(got, "R:GET") {
		t.Fatalf("got %s", got)
	}
}

func TestRedisTrackerQueuesResetAfterReplyOff(t *testing.T) {
	tracker := newRedisTracker("", false)
	got := redisTokens(redisClient(tracker, 1, redisStart(
		redisStep{true, redisArgs("CLIENT", "REPLY", "OFF"), 0},
		redisStep{true, redisArgs("RESET"), 0},
		redisStep{false, "+RESET\r\n", 0},
		redisStep{true, redisArgs("GET", "a"), 0},
		redisStep{false, "$1\r\n1\r\n", 0},
	)))
	if got != "C:PING R:PING/simple C:CLIENT REPLY C:RESET R:RESET/simple C:GET R:GET/bulk" {
		t.Fatalf("got %s", got)
	}
}

func TestRedisTrackerDoesNotHoldMonitorLines(t *testing.T) {
	// MONITOR는 trace 전에 시작했다. 줄이 record 둘로 나뉘어도 앞 record의 인자를 쥐고 있으면 안 된다.
	tracker := newRedisTracker("", true)
	redisClient(tracker, 1, redisStart(redisStep{true, redisArgs("PING"), 0}))
	tracker.events(redisRecord(1, false, false, 9_000_000, []byte("+1791788445.996979 [0 127.0.0.1:1] \"AUTH\" \"edc-pw"), 49), 0)
	redisWalk(reflect.ValueOf(tracker), map[uintptr]bool{}, func(s string) {
		if strings.Contains(s, "edc-pw") || strings.Contains(s, "AUTH") {
			t.Fatalf("tracker holds a MONITOR line: %q", s)
		}
	})
}

func TestRedisTrackerHidesGeneratedPasswords(t *testing.T) {
	tracker := newRedisTracker("", true)
	events := redisClient(tracker, 1, redisStart(
		redisStep{true, redisArgs("ACL", "GENPASS"), 0},
		redisStep{false, "$64\r\nedc-genpass-" + strings.Repeat("a", 52) + "\r\n", 0},
	))
	if text := fmt.Sprintf("%+v", *events[len(events)-1].redis); len(events) != 4 || strings.Contains(text, "edc-genpass") {
		t.Fatalf("events %d, reply %s", len(events), text)
	}
}

// redisFuzzRecords는 cuts의 6 byte 기술자대로 data를 record로 나눠 feed에 넘긴다. 남은 data는 요청 record 하나다.
func redisFuzzRecords(data, cuts []byte, limit int, forget func(), feed func(response bool, payload []byte, size int)) {
	pos := 0
	for i := 0; i+6 <= len(cuts) && i < 6*4096; i += 6 {
		cut := cuts[i : i+6]
		if cut[0]&2 != 0 {
			forget()
		}
		n := int(cut[1]) | int(cut[2])<<8
		hidden := int(cut[3]) | int(cut[4])<<8 | int(cut[5])<<16
		chunk := data[min(pos, len(data)):min(pos+n, len(data))]
		pos += len(chunk)
		if len(chunk)+hidden == 0 {
			continue
		}
		feed(cut[0]&1 != 0, bytes.Clone(chunk[:min(len(chunk), limit)]), len(chunk)+hidden)
	}
	if pos < len(data) {
		rest := data[pos:]
		feed(false, bytes.Clone(rest[:min(len(rest), limit)]), len(rest))
	}
}

// redisShowSecret는 show-secrets fuzz가 비밀 자리에만 심는 password다. seed에는 없다.
const redisShowSecret = "edc-show-secret-77"

// redisSecretTemplates는 password가 인자나 응답의 비밀 자리에 있는 요청과 응답이다. 빈 문자열은 그 방향 record가 없다는 뜻이다.
var redisSecretTemplates = func() [][2]string {
	pw := redisShowSecret
	return [][2]string{
		{redisArgs("AUTH", pw), "+OK\r\n"},
		{redisArgs("AUTH", "u", pw), "-WRONGPASS invalid username-password pair '" + pw + "'\r\n"},
		{redisArgs("HELLO", "3", "AUTH", "u", pw, "SETNAME", "c"), "%1\r\n$5\r\nproto\r\n:3\r\n"},
		{redisArgs("CONFIG", "SET", "requirepass", pw), "+OK\r\n"},
		{redisArgs("CONFIG", "GET", "requirepass"), "*2\r\n$11\r\nrequirepass\r\n$18\r\n" + pw + "\r\n"},
		{redisArgs("ACL", "SETUSER", "u", "on", ">"+pw), "+OK\r\n"},
		{redisArgs("ACL", "GENPASS"), "$18\r\n" + pw + "\r\n"},
		{redisArgs("MIGRATE", "h", "1", "k", "0", "1", "COPY", "AUTH2", "u", pw), "-IOERR error\r\n"},
		{redisArgs("SENTINEL", "SET", "m", "auth-pass", pw), "+OK\r\n"},
		{redisArgs("SENTINEL", "CONFIG", "SET", "sentinel-pass", pw), "+OK\r\n"},
		{"CONFIG \"SET\" requirepass \"" + pw + "\"\r\n", "+OK\r\n"},
		{"HELLO 3 'AUTH' \"us er\" " + pw + "\r\n", "-WRONGPASS bad\r\n"},
		{"AUTH \"" + pw + "\"\r\n", "+OK\r\n"},
		{redisArgs("PING"), "+1791788445.996979 [0 127.0.0.1:1] \"AUTH\" \"" + pw + "\"\r\n"},
	}
}()

// FuzzRedisTrackerShowSecrets는 --show-secrets에서도 비밀 자리의 password가 event와 tracker 상태에 남지 않는지 본다. fuzz는
// 비밀 요청과 응답을 나누는 자리와 캡처 길이, 그 뒤에 이어지는 record를 고른다.
func FuzzRedisTrackerShowSecrets(f *testing.F) {
	for i := range redisSecretTemplates {
		f.Add([]byte(nil), []byte(nil), uint16(4095), false, uint8(i), uint8(0), uint8(0))
		f.Add([]byte(redisMixed), redisFuzzCut(true, false, len(redisMixed), 0), uint16(1023), true, uint8(i), uint8(7), uint8(30))
	}
	f.Fuzz(func(t *testing.T, data, cuts []byte, capture uint16, server bool, template, requestCut, responseCut uint8) {
		if bytes.Contains(data, []byte(redisShowSecret)) {
			return
		}
		limit := int(capture)%4096 + 1
		tracker := newRedisTracker("", true)
		var events []redisTraced
		at := uint64(0)
		feed := func(response bool, payload []byte, size int) {
			at += 1_000_000
			events = append(events, tracker.events(redisRecord(1, server, response == server, at, payload, size), 0)...)
		}
		split := func(response bool, record string, cut uint8) {
			if record == "" {
				return
			}
			pos := int(cut) % len(record)
			for _, piece := range []string{record[:pos], record[pos:]} {
				if piece != "" {
					feed(response, []byte(piece[:min(len(piece), limit)]), len(piece))
				}
			}
		}
		pair := redisSecretTemplates[int(template)%len(redisSecretTemplates)]
		split(false, pair[0], requestCut)
		split(true, pair[1], responseCut)
		redisFuzzRecords(data, cuts, limit, func() { tracker.forgetSocket(1) }, feed)
		for _, traced := range events {
			for _, part := range []any{traced.event, traced.redis} {
				raw, err := json.Marshal(part)
				if err != nil || bytes.Contains(raw, []byte(redisShowSecret)) {
					t.Fatalf("event %s carries the password (%v)", raw, err)
				}
			}
		}
		redisWalk(reflect.ValueOf(tracker), map[uintptr]bool{}, func(s string) {
			if strings.Contains(s, redisShowSecret) {
				t.Fatalf("tracker state keeps the password: %q", s)
			}
		})
	})
}
