package edc

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"maps"
	"mime"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

// redirect 경로는 edc http check의 기본 출력이다. doctor는 -v일 때만 상세 기록을 켜므로, 기본 화면에는 evidence가 없다.
func init() { evidenceShownProbes["http.check"] = true }

// httpDetail은 http probe가 redirect 경로와 응답 내용을 얼마나 남기는지다. doctor와 watch는 같은 probe를
// 짧은 간격으로 여러 번 부르므로 기본값은 아무것도 더 남기지 않는다.
type httpDetail int

const (
	httpDetailOff httpDetail = iota
	// httpDetailHops는 redirect를 따라간 요청을 차례로 남긴다. edc http check의 기본이다.
	httpDetailHops
	// httpDetailFull은 요청마다 header를, 마지막 응답에는 본문 앞부분을 더한다. -v다.
	httpDetailFull
)

const (
	httpBodyPreviewBytes = 4 << 10
	httpBodyPreviewLines = 5
	// httpEvidenceIndent는 printResultDetail이 evidence 값 앞에 붙이는 칸 수다.
	httpEvidenceIndent = 8
	httpMinValueWidth  = 16
)

// httpHop은 응답 하나다. redirect를 따라가면 CheckRedirect가 다음 요청을 보내기 전에 앞 응답을 남긴다.
// 다음 요청의 GetConn이 시간 값을 지우기 때문이다.
type httpHop struct {
	url            string
	status         int
	proto          string
	peer           string
	proxy          bool
	reused         bool
	decompressed   bool
	timings        map[string]int64
	headers        http.Header
	requestHeaders http.Header
}

func newHTTPHop(request *http.Request, response *http.Response, peer string, proxy, reused bool, timings map[string]int64, detail httpDetail) httpHop {
	hop := httpHop{
		url: request.URL.String(), status: response.StatusCode, proto: response.Proto,
		peer: peer, proxy: proxy, reused: reused, decompressed: response.Uncompressed, timings: maps.Clone(timings),
	}
	if detail == httpDetailFull {
		hop.headers = response.Header.Clone()
		hop.requestHeaders = httpSentHeaders(request)
	}
	return hop
}

// httpSentHeaders는 이 요청에 실린 header다. -H로 넣은 것과 Go가 redirect에서 붙인 Referer가 들어 있다.
// Go는 다른 host로 redirect할 때 Authorization과 Cookie를 떼므로, hop마다 따로 읽어야 무엇이 빠졌는지 보인다.
func httpSentHeaders(request *http.Request) http.Header {
	sent := request.Header.Clone()
	if request.Host != "" && request.Host != request.URL.Host {
		sent.Set("Host", request.Host)
	}
	return sent
}

var httpTimingLabels = []struct{ key, label string }{{"dns_ms", "DNS"}, {"connect_ms", "TCP"}, {"tls_ms", "TLS"}, {"ttfb_ms", "TTFB"}}

func (hop httpHop) facts() string {
	var items []string
	for _, phase := range httpTimingLabels {
		if value, ok := hop.timings[phase.key]; ok {
			items = append(items, fmt.Sprintf("%s %dms", phase.label, value))
		}
	}
	if hop.reused {
		items = append(items, T("observe.http.reused"))
	}
	if hop.peer != "" {
		key := "observe.probe.peer_ip"
		if hop.proxy {
			key = "observe.probe.proxy_peer_ip"
		}
		items = append(items, T(key, hop.peer))
	}
	if hop.proto != "" {
		items = append(items, hop.proto)
	}
	if hop.decompressed {
		items = append(items, T("observe.http.decompressed"))
	}
	return strings.Join(items, " · ")
}

// formatHTTPHops는 요청을 보낸 순서대로 한 줄씩 status와 URL을 쓰고, 그 아래에 시간 값을 둔다.
// header는 -v에서만 그 아래에 붙는다.
func formatHTTPHops(hops []httpHop, width int) string {
	var builder strings.Builder
	numberWidth := len(strconv.Itoa(len(hops)))
	for index, hop := range hops {
		lead := fmt.Sprintf("%*d  %d  ", numberWidth, index+1, hop.status)
		pad := strings.Repeat(" ", len(lead))
		fmt.Fprintf(&builder, "%s%s\n", lead, traceEscapeText([]byte(hop.url)))
		if facts := hop.facts(); facts != "" {
			fmt.Fprintf(&builder, "%s%s\n", pad, facts)
		}
		for _, line := range httpHeaderLines(hop, width-httpEvidenceIndent-len(pad)) {
			fmt.Fprintf(&builder, "%s%s\n", pad, line)
		}
	}
	return strings.TrimRight(builder.String(), "\n")
}

// httpHeaderLines는 보낸 header를 >로, 받은 header를 그 아래에 쓴다. Go가 header 순서를 남기지 않으므로
// 이름순으로 정렬하고, HTTP/2처럼 소문자로 맞춘다. 긴 값은 폭에서 자르고 전체는 JSON에 남긴다.
func httpHeaderLines(hop httpHop, width int) []string {
	type row struct{ name, value string }
	var rows []row
	marker := ""
	if len(hop.requestHeaders) > 0 {
		marker = "  "
		for _, name := range sortedHeaderNames(hop.requestHeaders) {
			for _, value := range hop.requestHeaders.Values(name) {
				rows = append(rows, row{"> " + name, httpHeaderDisplayValue(name, value)})
			}
		}
	}
	for _, name := range sortedHeaderNames(hop.headers) {
		for _, value := range hop.headers.Values(name) {
			rows = append(rows, row{marker + name, httpHeaderDisplayValue(name, value)})
		}
	}
	nameWidth := 0
	for _, row := range rows {
		nameWidth = max(nameWidth, len(row.name))
	}
	valueWidth := max(width-nameWidth-2, httpMinValueWidth)
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		lines = append(lines, fmt.Sprintf("%-*s  %s", nameWidth, row.name, ansi.Truncate(row.value, valueWidth, "…")))
	}
	return lines
}

func sortedHeaderNames(header http.Header) []string {
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, strings.ToLower(name))
	}
	slices.Sort(names)
	return names
}

func httpSecretHeader(name string) bool {
	return slices.ContainsFunc(traceHTTPSecretHeaders, func(secret string) bool { return strings.EqualFold(name, secret) })
}

// httpHeaderDisplayValue는 인증 정보를 trace http와 같은 ***로 가린다. cookie는 이름과 속성을 남긴다.
// HttpOnly나 Secure가 붙었는지, 어떤 cookie가 오갔는지는 값 없이도 디버깅에 필요하다.
func httpHeaderDisplayValue(name, value string) string {
	switch {
	case strings.EqualFold(name, "set-cookie"):
		pair, attributes, hasAttributes := strings.Cut(value, ";")
		masked := httpMaskCookiePair(pair)
		if hasAttributes {
			masked += ";" + attributes
		}
		value = masked
	case strings.EqualFold(name, "cookie"):
		pairs := strings.Split(value, ";")
		for index, pair := range pairs {
			pairs[index] = httpMaskCookiePair(pair)
		}
		value = strings.Join(pairs, ";")
	case httpSecretHeader(name):
		value = "***"
	}
	return traceEscapeText([]byte(value))
}

func httpMaskCookiePair(pair string) string {
	cookie, _, ok := strings.Cut(pair, "=")
	if !ok {
		return "***"
	}
	return cookie + "=***"
}

// httpHeaderMetrics는 JSON에 남길 header다. 화면처럼 소문자 이름을 쓰고 인증 정보는 같은 방식으로 가린다.
func httpHeaderMetrics(header http.Header) map[string][]string {
	values := make(map[string][]string, len(header))
	for name, list := range header {
		lower := strings.ToLower(name)
		for _, value := range list {
			values[lower] = append(values[lower], httpHeaderDisplayValue(lower, value))
		}
	}
	return values
}

func httpHopMetrics(hops []httpHop) []map[string]interface{} {
	rows := make([]map[string]interface{}, 0, len(hops))
	for _, hop := range hops {
		row := map[string]interface{}{"url": hop.url, "status_code": hop.status, "proto": hop.proto}
		for key, value := range hop.timings {
			row[key] = value
		}
		if hop.peer != "" {
			row["peer_ip"] = hop.peer
		}
		if hop.proxy {
			row["peer_is_proxy"] = true
		}
		if hop.reused {
			row["reused_connection"] = true
		}
		if hop.decompressed {
			row["decompressed"] = true
		}
		if hop.headers != nil {
			row["headers"] = httpHeaderMetrics(hop.headers)
		}
		if len(hop.requestHeaders) > 0 {
			row["request_headers"] = httpHeaderMetrics(hop.requestHeaders)
		}
		rows = append(rows, row)
	}
	return rows
}

// httpBodyCounter는 본문을 버리면서 크기와 줄 수를 세고, 앞부분만 미리보기로 남긴다.
type httpBodyCounter struct {
	keep     int
	size     int64
	newlines int64
	last     byte
	preview  []byte
}

func (counter *httpBodyCounter) Write(data []byte) (int, error) {
	if room := counter.keep - len(counter.preview); room > 0 {
		counter.preview = append(counter.preview, data[:min(room, len(data))]...)
	}
	counter.size += int64(len(data))
	counter.newlines += int64(bytes.Count(data, []byte{'\n'}))
	if len(data) > 0 {
		counter.last = data[len(data)-1]
	}
	return len(data), nil
}

func (counter *httpBodyCounter) lines() int64 {
	if counter.size > 0 && counter.last != '\n' {
		return counter.newlines + 1
	}
	return counter.newlines
}

// httpTextMedia는 미리보기로 보여 줄 수 있는 content type이다. 그 밖은 binary로 보고 크기만 쓴다.
func httpTextMedia(contentType, contentEncoding string, preview []byte) bool {
	if contentEncoding != "" && !strings.EqualFold(contentEncoding, "identity") {
		return false
	}
	if contentType == "" {
		return utf8.Valid(preview) && !bytes.ContainsRune(preview, 0)
	}
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch {
	case strings.HasPrefix(media, "text/"), strings.HasSuffix(media, "+json"), strings.HasSuffix(media, "+xml"):
		return true
	}
	switch media {
	case "application/json", "application/xml", "application/javascript", "application/x-www-form-urlencoded", "application/xhtml+xml":
		return true
	}
	return false
}

var httpTitlePattern = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

func httpHTMLTitle(contentType string, preview []byte) string {
	media, _, _ := mime.ParseMediaType(contentType)
	if media != "text/html" && media != "application/xhtml+xml" {
		return ""
	}
	match := httpTitlePattern.FindSubmatch(preview)
	if match == nil {
		return ""
	}
	return strings.Join(strings.Fields(html.UnescapeString(string(match[1]))), " ")
}

// formatHTTPBody는 마지막 응답의 종류와 크기, HTML 제목, 앞쪽 몇 줄이다. 본문은 상대가 보낸 값이라
// terminal escape를 실행하지 않도록 traceEscapeText를 거친다.
func formatHTTPBody(header http.Header, counter *httpBodyCounter, width int) string {
	contentType := header.Get("Content-Type")
	kind := contentType
	if kind == "" {
		kind = T("observe.http.no_content_type")
	}
	kind = traceEscapeText([]byte(kind))
	lineWidth := max(width-httpEvidenceIndent, httpMinValueWidth)
	encoding := header.Get("Content-Encoding")
	if !httpTextMedia(contentType, encoding, counter.preview) {
		lines := []string{T("observe.http.body_size", kind, counter.size)}
		switch {
		case counter.size == 0:
		case encoding != "" && !strings.EqualFold(encoding, "identity"):
			lines = append(lines, T("observe.http.encoded", traceEscapeText([]byte(encoding))))
		default:
			lines = append(lines, T("observe.http.binary"))
		}
		return strings.Join(lines, "\n")
	}
	lines := []string{T("observe.http.body_meta", kind, counter.size, counter.lines())}
	if title := httpHTMLTitle(contentType, counter.preview); title != "" {
		lines = append(lines, ansi.Truncate(T("observe.http.title", traceEscapeText([]byte(title))), lineWidth, "…"))
	}
	preview := strings.Split(strings.TrimRight(traceEscapeText(counter.preview), "\n"), "\n")
	for index, line := range preview {
		if index == httpBodyPreviewLines {
			lines = append(lines, "…")
			break
		}
		lines = append(lines, ansi.Truncate(strings.ReplaceAll(line, "\t", "  "), lineWidth, "…"))
	}
	return strings.Join(lines, "\n")
}

// httpBodyLimit은 본문을 읽는 상한이다. --body로 저장할 때도 같고, 넘으면 잘렸다고 알린다.
const httpBodyLimit = 10 << 20

// httpBodyFile은 --body가 가리키는 곳이다. 쓰기 오류를 따로 남겨, 응답을 읽다 난 오류와 구분한다.
type httpBodyFile struct {
	path      string
	file      *os.File
	err       error
	truncated bool
	// escaped는 --body -의 stdout이 터미널인 경우다. 상대가 보낸 escape sequence가 화면이나 클립보드를 조작하지
	// 못하게, 본문을 모았다가 제어 문자를 \xNN으로 바꿔 쓴다. 파이프와 파일에는 받은 바이트를 그대로 쓴다.
	escaped bool
	pending bytes.Buffer
}

// openHTTPBodyFile은 응답을 받은 뒤에 연다. 요청이 실패하면 빈 파일이 남지 않는다. curl -o처럼 있던 파일은 덮어쓴다.
func openHTTPBodyFile(path string) (*httpBodyFile, error) {
	if path == "-" {
		return &httpBodyFile{path: path, file: os.Stdout, escaped: isTerminal(os.Stdout)}, nil
	}
	file, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &httpBodyFile{path: path, file: file}, nil
}

func (saved *httpBodyFile) Write(data []byte) (int, error) {
	if saved.escaped {
		return saved.pending.Write(data)
	}
	written, err := saved.file.Write(data)
	if err != nil && saved.err == nil {
		saved.err = err
	}
	return written, err
}

func (saved *httpBodyFile) close() error {
	if saved.escaped {
		if _, err := io.WriteString(saved.file, traceEscapeText(saved.pending.Bytes())); err != nil && saved.err == nil {
			saved.err = err
		}
		return nil
	}
	if saved.file == os.Stdout {
		return nil
	}
	return saved.file.Close()
}

// httpBodyContinues는 상한까지 읽은 뒤에도 본문이 남았는지 본다.
func httpBodyContinues(body io.Reader) bool {
	var next [1]byte
	read, _ := io.ReadFull(body, next[:])
	return read > 0
}

// withHTTPBodyOutput은 본문을 어디에 썼는지 남긴다. -v의 본문 블록이 있으면 그 끝에 붙인다.
func withHTTPBodyOutput(result Result, saved *httpBodyFile, size int64) Result {
	if saved == nil {
		return result
	}
	line := T("observe.http.body_saved", saved.path, size)
	if saved.path == "-" {
		line = T("observe.http.body_stdout", size)
	}
	result.Metrics["body_output"] = saved.path
	if saved.truncated {
		result.Metrics["body_truncated"] = true
		line += "\n" + T("observe.http.body_truncated", fmt.Sprintf("%d MiB", httpBodyLimit>>20))
	}
	label := T("observe.http.body")
	for index := range result.Evidence {
		if result.Evidence[index].Label == label {
			result.Evidence[index].Value += "\n" + line
			return result
		}
	}
	result.Evidence = append(result.Evidence, Evidence{Label: label, Value: line})
	return result
}

// httpHeaderFlags는 curl의 -H처럼 반복해서 받는 요청 header다.
type httpHeaderFlags []string

func (flags *httpHeaderFlags) String() string { return strings.Join(*flags, ", ") }

func (flags *httpHeaderFlags) Set(value string) error {
	name, field, ok := strings.Cut(value, ":")
	name = strings.TrimSpace(name)
	// 값에 줄바꿈이 들어가면 header 하나가 두 줄이 되어 다른 header나 본문을 끼워 넣을 수 있다.
	if !ok || !httpTokenName(name) || !httpFieldValue(field) {
		return fmt.Errorf("%s", T("cli.error.http_header"))
	}
	*flags = append(*flags, name+": "+strings.TrimSpace(field))
	return nil
}

func (flags httpHeaderFlags) header() http.Header {
	header := http.Header{}
	for _, line := range flags {
		name, value, _ := strings.Cut(line, ": ")
		header.Add(name, value)
	}
	return header
}

// httpFieldValue는 header 값에 쓸 수 있는 글자인지다. Go는 제어 문자가 든 값을 보내지 않고 요청 전체를 실패시켜,
// 사용 오류가 네트워크 실패로 보고되었다. tab만 허용한다.
func httpFieldValue(value string) bool {
	for index := 0; index < len(value); index++ {
		if character := value[index]; (character < 0x20 && character != '\t') || character == 0x7f {
			return false
		}
	}
	return true
}

// httpTrimPartialRune은 상한에서 자른 미리보기 끝의 덜 끝난 UTF-8 문자를 버린다. 남겨 두면 Content-Type이 없는
// 텍스트가 binary로 판정되고, 끝에 \xNN이 붙는다.
func httpTrimPartialRune(preview []byte) []byte {
	for back := 1; back <= utf8.UTFMax && back <= len(preview); back++ {
		start := len(preview) - back
		if utf8.RuneStart(preview[start]) {
			if !utf8.FullRune(preview[start:]) {
				return preview[:start]
			}
			return preview
		}
	}
	return preview
}

// httpOutputWidth는 결과를 쓸 터미널의 폭이다. --body -이면 본문이 stdout을 쓰고 결과는 stderr로 간다.
func httpOutputWidth(options httpCheckOptions) int {
	if options.bodyPath != "-" {
		return terminalWidth()
	}
	if width, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && width > 0 {
		return width
	}
	return remoteDefaultWidth
}

// httpTokenName은 RFC 9110의 token이다. Go는 이름이 잘못된 header를 보내지 않고 요청 전체를 실패시키므로
// flag를 읽을 때 먼저 막는다.
func httpTokenName(name string) bool {
	if name == "" {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", character) >= 0:
		default:
			return false
		}
	}
	return true
}
