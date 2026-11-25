package edc

import (
	"errors"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// listenSocket은 연결을 기다리는 소켓 하나다. 사람이 묻는 것은 "무엇이 열려 있나"이므로 포트와
// 그 포트를 잡은 프로세스를 함께 담는다.
type listenSocket struct {
	Proto   string `json:"proto"`
	Address string `json:"address"`
	// Port는 tcp와 udp에만 있다. unix 소켓은 포트가 아니라 경로를 가지므로 Address에 경로가 들어간다.
	Port    int    `json:"port,omitempty"`
	Process string `json:"process"`
	PID     string `json:"pid"`

	// 아래 세 값은 -v에서만 화면에 나온다. JSON에는 언제나 담는다. 도구가 주지 않는 값은 비운다.
	User string `json:"user,omitempty"`
	FD   string `json:"fd,omitempty"`
	// Queue는 ss가 Recv-Q/Send-Q로 적는 값이다. TCP listener에서는 accept 큐의 깊이와 한계이고, UDP에서는
	// 수신·송신 버퍼의 바이트다. 0도 뜻이 있으므로 아는지 여부를 따로 둔다.
	RecvQ    int  `json:"recv_q"`
	SendQ    int  `json:"send_q"`
	HasQueue bool `json:"has_queue"`

	// Args와 Cwd는 같은 이름의 process 여럿을 가르는 단서다. process마다 따로 읽어야 해서 --full과 --json일
	// 때만 채운다. Args의 비밀 값은 담기 전에 가린다.
	Args []string `json:"args,omitempty"`
	Cwd  string   `json:"cwd,omitempty"`
	// Container는 이 포트를 publish한 container다. 소켓의 주인은 OrbStack Helper나 docker-proxy처럼 대신 연 process다.
	Container *listenContainer `json:"container,omitempty"`

	// inode는 Linux에서 소켓을 /proc/*/fd의 주인과 잇는 열쇠다. 출력하지 않는다.
	inode uint32
}

// listenSocketKey는 사람이 보기에 같은 소켓인지를 정한다. fd나 큐 깊이는 같은 소켓의 순간 상태일
// 뿐이므로 열쇠에 넣지 않는다. 넣으면 -v를 줄 때 개수가 달라져 -v가 데이터를 바꾸게 된다.
type listenSocketKey struct {
	Proto, Address string
	Port           int
	Process, PID   string
}

func listenBindingScope(proto, address string) string {
	if proto == "unix" {
		return "unix"
	}
	if proto != "tcp" && proto != "udp" {
		return "unknown"
	}
	if address == "*" {
		return "all"
	}
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return "unknown"
	}
	ip = ip.Unmap()
	if ip.IsUnspecified() {
		return "all"
	}
	if ip.IsLoopback() {
		return "loopback"
	}
	return "specific"
}

func listenKeyOf(socket listenSocket) listenSocketKey {
	return listenSocketKey{socket.Proto, socket.Address, socket.Port, socket.Process, socket.PID}
}

// listenRow는 화면의 한 줄이다. 포트, 프로세스, 바인딩 범위, container가 같고 주소나 PID만 다른 소켓을 모은다. IPv4와
// IPv6로 함께 연 포트나 127.0.0.53과 127.0.0.54처럼 범위가 같은 주소 여럿이 한 줄이 된다. JSON은 소켓 단위 그대로 둔다.
type listenRow struct {
	Sockets []listenSocket
}

// listenRowKey는 한 줄로 모을 소켓을 정한다. 범위를 넣어 loopback과 노출된 주소가 한 줄에 섞이지 않게 한다.
type listenRowKey struct {
	Proto, Process, Scope string
	Port                  int
	// Address와 PID는 포트가 없는 유닉스 소켓에만 채운다. 유닉스 소켓은 모으지 않는다.
	Address, PID string
	Container    listenContainer
	Published    bool
}

func listenRowKeyOf(socket listenSocket) listenRowKey {
	key := listenRowKey{Proto: socket.Proto, Process: socket.Process, Scope: listenBindingScope(socket.Proto, socket.Address), Port: socket.Port}
	if socket.Port == 0 {
		key.Address, key.PID = socket.Address, socket.PID
	}
	if socket.Container != nil {
		key.Container, key.Published = *socket.Container, true
	}
	return key
}

// groupListenSockets는 소켓을 화면의 줄로 모은다. 줄의 순서는 각 줄의 첫 소켓이 나온 순서다. 주소 순으로 정렬한 목록에서는
// 같은 줄의 소켓 사이에 다른 process의 소켓이 끼어 있을 수 있어서, 이웃한 소켓만 모으지 않는다.
func groupListenSockets(sockets []listenSocket) []listenRow {
	index := map[listenRowKey]int{}
	var rows []listenRow
	for _, socket := range sockets {
		key := listenRowKeyOf(socket)
		if at, found := index[key]; found {
			rows[at].Sockets = append(rows[at].Sockets, socket)
			continue
		}
		index[key] = len(rows)
		rows = append(rows, listenRow{Sockets: []listenSocket{socket}})
	}
	return rows
}

func (row listenRow) first() listenSocket { return row.Sockets[0] }

func (row listenRow) key() listenRowKey { return listenRowKeyOf(row.first()) }

func (row listenRow) scope() string {
	return listenBindingScope(row.first().Proto, row.first().Address)
}

// values는 소켓마다 읽은 값을 처음 나온 순서로, 겹치지 않게 모은다. 빈 값은 뺀다.
func (row listenRow) values(read func(listenSocket) string) []string {
	var values []string
	for _, socket := range row.Sockets {
		if value := read(socket); value != "" && !slices.Contains(values, value) {
			values = append(values, value)
		}
	}
	return values
}

func (row listenRow) addresses() []string {
	return row.values(func(socket listenSocket) string { return socket.Address })
}

func (row listenRow) pids() []string {
	return row.values(func(socket listenSocket) string { return socket.PID })
}

// joined는 소켓마다 다른 값을 ", "로 잇는다. 값이 없으면 "-"다.
func (row listenRow) joined(read func(listenSocket) string) string {
	return listenCell(strings.Join(row.values(read), ", "))
}

// listenScopeColor는 바인딩 범위의 색이다. 모든 주소에 바인딩한 소켓은 밖에서 닿을 수 있는 후보라 가장 눈에 띄게 둔다.
func listenScopeColor(scope string) string {
	switch scope {
	case "all":
		return richYellow
	case "specific":
		return richCyan
	case "loopback":
		return richGray
	}
	return ""
}

// listenFamilies는 어떤 소켓 종류를 볼지 정한다. 플래그는 더하기가 아니라 필터이고, 아무것도 주지
// 않으면 포트를 가진 종류를 모두 본다. 포트를 가진 것을 기본에서 숨기면 "열린 포트"를 묻는 사람이
// 답을 못 얻는다.
type listenFamilies struct {
	TCP  bool
	UDP  bool
	Unix bool
}

// defaultListenFamilies는 종류를 고르지 않았을 때 보는 집합이다. 포트를 가진 종류는 모두 넣는다.
// 유닉스 소켓은 포트가 없어 표의 주 열이 비고, macOS에서는 상태를 알 수 없어 근사치라 뺀다.
func defaultListenFamilies() listenFamilies {
	return listenFamilies{TCP: true, UDP: true}
}

// selectedListenFamilies는 플래그를 집합으로 바꾼다. 하나라도 고르면 고른 것만 남고, 아무것도 고르지
// 않으면 기본 집합이다. --all은 유닉스까지 포함한 전부다.
func selectedListenFamilies(tcp, udp, unix, all bool) listenFamilies {
	if all {
		return listenFamilies{TCP: true, UDP: true, Unix: true}
	}
	if !tcp && !udp && !unix {
		return defaultListenFamilies()
	}
	return listenFamilies{TCP: tcp, UDP: udp, Unix: unix}
}

func (families listenFamilies) wants(proto string) bool {
	switch proto {
	case "tcp":
		return families.TCP
	case "udp":
		return families.UDP
	case "unix":
		return families.Unix
	default:
		return false
	}
}

// userNames는 같은 uid를 되풀이해 찾지 않으려고 둔다. 한 호스트의 소켓 수십 개가 몇 개의 계정으로
// 모이므로 조회는 그 수만큼만 일어난다.
var userNames = map[string]string{}

// lookupUserName은 uid를 계정 이름으로 바꾼다. 이름을 못 찾으면 번호를 그대로 쓴다. 번호는 틀린
// 이름보다 낫고, 사라진 계정이 포트를 잡고 있다는 사실 자체가 단서다.
func lookupUserName(uid string) string {
	if name, found := userNames[uid]; found {
		return name
	}
	name := uid
	if account, err := user.LookupId(uid); err == nil && account.Username != "" {
		name = account.Username
	}
	userNames[uid] = name
	return name
}

// dedupeListenSockets는 같은 소켓을 한 줄로 모은다. 한 포트를 IPv4와 IPv6로 함께 열면 도구가 두 줄을
// 내지만 사람에게는 같은 사실이다.
func dedupeListenSockets(sockets []listenSocket) []listenSocket {
	seen := map[listenSocketKey]bool{}
	unique := make([]listenSocket, 0, len(sockets))
	for _, socket := range sockets {
		key := listenKeyOf(socket)
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, socket)
	}
	sort.Slice(unique, func(i, j int) bool {
		// 포트가 없는 유닉스 소켓을 뒤로 보낸다. 앞에 두면 --all에서 포트 목록이 아래로 밀린다.
		if (unique[i].Port == 0) != (unique[j].Port == 0) {
			return unique[j].Port == 0
		}
		if unique[i].Port != unique[j].Port {
			return unique[i].Port < unique[j].Port
		}
		if unique[i].Proto != unique[j].Proto {
			return unique[i].Proto < unique[j].Proto
		}
		return unique[i].Address < unique[j].Address
	})
	return unique
}

// formatListenTable은 포트를 앞에 두고 그 포트를 잡은 프로세스를 함께 그린다. 이 명령에 사람이 묻는
// 것이 "어느 포트가 열려 있나"이므로 포트가 첫 열이다.
//
// detail은 열을 더 여는 것일 뿐 줄을 늘리거나 줄이지 않는다. -v가 목록 자체를 바꾸면 -v를 준 화면과
// 주지 않은 화면이 다른 사실을 말하게 된다.
//
// note가 있으면 각 행 아래에 명령줄을 note로 칠한 줄로 붙인다. 명령줄은 길이가 process마다 크게 달라 열로
// 두면 표 폭이 가장 긴 명령줄을 따라간다.
//
// 주소만 다른 소켓은 한 행에 모은다(listenRow). -v의 USER, FD, QUEUE도 행 안에서 값을 이어 적어 행 수가 -v와 무관하게
// 같다. style은 ADDRESS와 SCOPE 칸을 범위의 색으로 칠한다. 색이 없어도 SCOPE 칸이 범위를 글자로 남긴다.
//
// ADDRESS 칸은 가장 긴 주소 하나의 폭이다. 한 행의 주소가 그 폭을 넘으면 넘는 주소를 아랫줄에 적는다. 긴 IPv6 주소 쌍
// 하나가 모든 행을 넓혀 PROCESS 칸을 화면 밖으로 밀어내지 않게 한다.
func formatListenTable(sockets []listenSocket, detail bool, style textStyle, note func(string) string) string {
	return formatListenTableWith(sockets, detail, style, note, nil)
}

// formatListenTableWith는 주소 칸의 글자를 address로 바꿔 적는다. --redact가 다 짠 표에서 주소를 가리면 가린 글자의 길이가
// 달라 칸이 어긋나므로, 폭을 재기 전에 바꾼다. 행을 모으는 기준과 범위는 바꾸기 전의 주소로 정한다.
func formatListenTableWith(sockets []listenSocket, detail bool, style textStyle, note func(string) string, address func(string) string) string {
	shown := func(value string) string {
		if address == nil {
			return value
		}
		return address(value)
	}
	headers := []string{T("observe.listen.column.port"), T("observe.listen.column.proto"), T("observe.listen.column.address"), T("observe.listen.column.scope"), T("observe.listen.column.process")}
	if detail {
		headers = append(headers, T("observe.listen.column.user"), T("observe.listen.column.fd"), T("observe.listen.column.queue"))
	}
	home, _ := os.UserHomeDir()
	grouped := groupListenSockets(sockets)
	widest := 0
	for _, socket := range sockets {
		widest = max(widest, liveWidth(shown(socket.Address)))
	}
	rows := make([][]string, 0, len(grouped))
	commands := make([][]string, 0, len(grouped))
	addressLines := make([][]string, 0, len(grouped))
	for _, group := range grouped {
		socket := group.first()
		if note != nil {
			commands = append(commands, listenRowCommandLines(group, home))
		}
		process := socket.Process
		if pids := group.pids(); len(pids) > 0 {
			process += style.dim(" (" + strings.Join(pids, ", ") + ")")
		}
		if container := socket.Container; container != nil {
			process += style.paint(" → "+topPrintableText(listenContainerLabel(*container)), richCyan)
		}
		port := strconv.Itoa(socket.Port)
		if socket.Port == 0 {
			// 유닉스 소켓은 포트가 없다. 빈 칸 대신 없음을 그대로 적는다.
			port = "-"
		}
		color := listenScopeColor(group.scope())
		addresses := group.addresses()
		for index := range addresses {
			addresses[index] = shown(addresses[index])
		}
		addresses = packListenAddresses(addresses, widest)
		for index := range addresses {
			addresses[index] = style.paint(addresses[index], color)
		}
		addressLines = append(addressLines, addresses[1:])
		row := []string{port, socket.Proto, addresses[0], style.paint(T("observe.listen.scope."+group.scope()), color), process}
		if detail {
			row = append(row, group.joined(func(socket listenSocket) string { return socket.User }), group.joined(func(socket listenSocket) string { return socket.FD }), group.joined(listenQueueValue))
		}
		rows = append(rows, row)
	}
	widths := make([]int, len(headers))
	for index, header := range headers {
		widths[index] = liveWidth(header)
	}
	for _, row := range rows {
		for index, cell := range row {
			if width := liveWidth(cell); width > widths[index] {
				widths[index] = width
			}
		}
	}
	var builder strings.Builder
	writeRow := func(cells []string) {
		for index, cell := range cells {
			if index == len(cells)-1 {
				builder.WriteString(cell)
				break
			}
			builder.WriteString(liveCell(cell, widths[index]+2))
		}
		builder.WriteString("\n")
	}
	writeRow(headers)
	for index, row := range rows {
		writeRow(row)
		for _, line := range addressLines[index] {
			builder.WriteString(strings.Repeat(" ", widths[0]+2+widths[1]+2) + line + "\n")
		}
		if note != nil {
			for _, command := range commands[index] {
				builder.WriteString(strings.Repeat(" ", widths[0]+2) + note("↳ "+command) + "\n")
			}
		}
		if container := grouped[index].first().Container; note != nil && container != nil {
			line := T("observe.listen.container_port", container.Port, container.Proto, container.Image)
			if container.Command != "" {
				line += ": " + container.Command
			}
			builder.WriteString(strings.Repeat(" ", widths[0]+2) + note("↳ "+topPrintableText(line)) + "\n")
		}
	}
	return builder.String()
}

// listenRowCommandLines는 행 아래에 붙일 명령줄이다. 같은 줄은 한 번만 적는다. 한 행의 process들이 서로 다른 명령줄로
// 떠 있으면 줄마다 그 PID를 앞에 적어 어느 process의 것인지 가린다.
func listenRowCommandLines(row listenRow, home string) []string {
	var lines []string
	owners := map[string][]string{}
	for _, socket := range row.Sockets {
		line := listenCommandLine(socket, home)
		if line == "" {
			continue
		}
		if _, seen := owners[line]; !seen {
			lines = append(lines, line)
		}
		if socket.PID != "" && !slices.Contains(owners[line], socket.PID) {
			owners[line] = append(owners[line], socket.PID)
		}
	}
	if len(lines) < 2 {
		return lines
	}
	for index, line := range lines {
		lines[index] = listenCell(strings.Join(owners[line], ", ")) + "   " + line
	}
	return lines
}

// packListenAddresses는 주소를 ", "로 이으면서 width를 넘기 전에 줄을 나눈다. 나눈 줄의 끝에는 ","를 남긴다.
func packListenAddresses(addresses []string, width int) []string {
	if len(addresses) == 0 {
		return []string{"-"}
	}
	var lines []string
	current := addresses[0]
	for _, address := range addresses[1:] {
		if next := current + ", " + address; liveWidth(next) <= width {
			current = next
			continue
		}
		lines = append(lines, current+",")
		current = address
	}
	return append(lines, current)
}

// listenInterpreters는 첫 인자가 실제 프로그램인 실행 파일이다. node 여럿은 이름으로 가를 수 없고 스크립트
// 이름으로 갈린다. bun과 deno는 첫 인자가 run 같은 하위 명령이라 넣지 않는다.
var listenInterpreters = map[string]bool{"node": true, "python": true, "ruby": true, "perl": true, "php": true}

// listenCommandLine은 행 아래에 둘 작업 디렉터리와 명령줄이다. 인터프리터는 스크립트 이름을 앞에 두고, 홈 경로는
// ~로 줄인다. 표의 PROCESS 칸보다 더 알려 주는 것이 없으면 빈 문자열이다.
func listenCommandLine(socket listenSocket, home string) string {
	if len(socket.Args) == 0 && socket.Cwd == "" {
		return ""
	}
	var words []string
	if len(socket.Args) != 0 {
		program, args := socket.Args[0], socket.Args[1:]
		// nginx처럼 argv[0]을 "nginx: master process …"로 바꿔 쓰는 process가 있어서 경로일 때만 마지막 요소를 남긴다.
		if strings.HasPrefix(program, "/") {
			program = filepath.Base(program)
		}
		if listenInterpreters[strings.TrimRight(program, "0123456789.")] && len(args) != 0 && !strings.HasPrefix(args[0], "-") {
			program, args = filepath.Base(args[0]), args[1:]
		}
		words = append(words, program)
		for _, arg := range args {
			// Electron 앱은 빈 인자를 여럿 넘긴다. 따옴표 쌍만 늘어놓아 봐야 가르는 데 보탬이 없다.
			if arg == "" {
				continue
			}
			arg = listenShortPath(arg, home)
			if strings.ContainsAny(arg, " \t") {
				arg = strconv.Quote(arg)
			}
			words = append(words, arg)
		}
	}
	// Linux의 comm은 15자에서 잘려서 실행 파일 이름이 PROCESS 칸으로 시작하기만 해도 같은 process다. nginx처럼 바꿔 쓴
	// 제목은 공백이 들어 있고 PROCESS 칸에 없는 상태를 담는다.
	if len(words) == 1 && (words[0] == socket.Process || socket.Process != "" && strings.HasPrefix(words[0], socket.Process) && !strings.Contains(words[0], " ")) {
		words = nil
	}
	line := strings.Join(words, " ")
	// 작업 디렉터리는 앞에 둔다. 같은 명령 여럿을 가르는 단서인데, 끝에 두면 긴 명령줄이 폭에 잘릴 때 먼저 사라진다.
	// launchd와 systemd가 띄운 process는 작업 디렉터리가 /라서 가르는 데 쓸모가 없다.
	if socket.Cwd != "" && socket.Cwd != "/" {
		line = strings.TrimSpace(listenShortPath(socket.Cwd, home) + "   " + line)
	}
	return topPrintableText(line)
}

// listenProcessLabel은 탐색 목록의 PROCESS 칸에서 이름 뒤에 붙일 짧은 꼬리표다. node처럼 이름만으로는 무엇을 띄웠는지
// 알 수 없는 인터프리터에만 붙인다. 스크립트 파일 이름과 작업 디렉터리의 마지막 이름만 쓰고 인자는 넣지 않는다. 인자에는
// 가리지 못한 비밀이 남을 수 있고, 목록은 --full 없이도 보이기 때문이다.
func listenProcessLabel(socket listenSocket, home string) string {
	if len(socket.Args) == 0 || !listenInterpreters[strings.TrimRight(socket.Process, "0123456789.")] {
		return ""
	}
	program, args := socket.Args[0], socket.Args[1:]
	label := ""
	switch {
	case listenInterpreters[strings.TrimRight(filepath.Base(program), "0123456789.")]:
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			return ""
		}
		label = filepath.Base(args[0])
		if listenLooksSecret(label) {
			return ""
		}
	// Raycast Backend처럼 argv[0]을 제목으로 바꿔 쓴 process는 그 제목이 무엇인지 알려 준다. 제목도 process가 마음대로
	// 쓰므로, 비밀 이름이나 URL 자격 증명처럼 보이는 낱말이 있으면 보이지 않는다.
	case !strings.HasPrefix(program, "/"):
		if listenLooksSecret(program) {
			return ""
		}
		return topPrintableText(program)
	default:
		return ""
	}
	if socket.Cwd != "" && socket.Cwd != "/" && socket.Cwd != home && !listenLooksSecret(filepath.Base(socket.Cwd)) {
		label += " (" + filepath.Base(socket.Cwd) + ")"
	}
	return topPrintableText(label)
}

// listenLooksSecret은 꼬리표에 쓸 낱말에 비밀 이름이나 URL 자격 증명처럼 보이는 것이 있는지 본다.
func listenLooksSecret(text string) bool {
	return listenURLCredential.MatchString(text) || slices.ContainsFunc(strings.FieldsFunc(text, func(r rune) bool { return r == ' ' || r == '=' || r == ':' }), listenSecretFlag.MatchString)
}

func listenShortPath(text, home string) string {
	if home == "" || home == "/" {
		return text
	}
	if text == home {
		return "~"
	}
	return strings.ReplaceAll(text, home+"/", "~/")
}

var (
	// redis의 requirepass와 masterauth는 pass 앞에 구분자가 없어 이름을 따로 둔다. --passive 같은 옵션은 가리지 않는다.
	listenSecretFlag    = regexp.MustCompile(`(?i)(^|[-_.])(pass(word|wd)?|requirepass|masterauth|secret|token|api[-_]?key|credentials?)s?($|[-_.])`)
	listenURLCredential = regexp.MustCompile(`(://[^/:@\s]*:)[^/@\s]+@`)
)

// listenMaskArgs는 명령줄의 비밀 값을 가린다. listen 출력은 이슈와 history에 그대로 옮겨지는 일이 많아서
// --redact를 주지 않아도 가린다. 이름에 비밀을 뜻하는 낱말이 든 옵션의 값과 URL의 암호가 대상이다.
func listenMaskArgs(args []string) []string {
	masked := slices.Clone(args)
	for index := range masked {
		masked[index] = listenURLCredential.ReplaceAllString(masked[index], "${1}***@")
	}
	for index := 1; index < len(masked); index++ {
		arg := masked[index]
		hasValue, secret := listenSecretOption(arg)
		if !secret {
			continue
		}
		if hasValue {
			masked[index] = arg[:strings.IndexByte(arg, '=')+1] + "***"
			continue
		}
		// 값이 -로 시작할 수 있어서(base64url 토큰) 다음 인자가 옵션처럼 보여도 가린다. 불리언 옵션 뒤의 옵션을
		// 가리는 쪽이 비밀을 내보내는 쪽보다 낫다. 가린 인자가 값 없는 비밀 옵션이면 --password --token abc123의
		// abc123처럼 그 값이 남으므로 이어서 가린다.
		for index+1 < len(masked) {
			index++
			next := masked[index]
			masked[index] = "***"
			if hasValue, secret := listenSecretOption(next); !secret || hasValue {
				break
			}
		}
	}
	return masked
}

func listenSecretOption(arg string) (hasValue, secret bool) {
	if !strings.HasPrefix(arg, "-") {
		return false, false
	}
	name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
	return hasValue, listenSecretFlag.MatchString(name)
}

// attachListenCommands는 소켓마다 주인 process의 명령줄과 작업 디렉터리를 채운다. 한 process가 소켓 여럿을
// 가지는 일이 흔해 process마다 한 번만 읽는다.
func attachListenCommands(sockets []listenSocket) {
	type command struct {
		args []string
		cwd  string
	}
	commands := map[string]command{}
	for index := range sockets {
		pid := sockets[index].PID
		if pid == "" {
			continue
		}
		found, cached := commands[pid]
		if !cached {
			args, cwd := listenProcessCommand(pid)
			found = command{listenMaskArgs(args), cwd}
			commands[pid] = found
		}
		sockets[index].Args, sockets[index].Cwd = found.args, found.cwd
	}
}

// listenCell은 도구가 주지 않은 값을 빈 칸 대신 없음으로 그린다. 빈 칸은 값이 0인지 모르는지를
// 구별해 주지 않는다.
func listenCell(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// listenQueueCell은 Recv-Q/Send-Q를 한 칸에 적는다.
func listenQueueCell(socket listenSocket) string {
	return listenCell(listenQueueValue(socket))
}

// listenQueueValue는 큐를 모르면 빈 문자열이다. 한 행에 모은 소켓의 큐를 이을 때 모르는 값을 뺀다.
func listenQueueValue(socket listenSocket) string {
	if !socket.HasQueue {
		return ""
	}
	return strconv.Itoa(socket.RecvQ) + "/" + strconv.Itoa(socket.SendQ)
}

func validListenBrowseMode(mode string) bool {
	return mode == "auto" || mode == "always" || mode == "never"
}

func resolveListenBrowse(mode string, override *bool, terminal, watch, jsonOutput bool) (bool, error) {
	if override != nil {
		if !*override {
			return false, nil
		}
		if watch || jsonOutput {
			return false, errors.New(T("listenbrowse.conflict"))
		}
		mode = "always"
	}
	if watch || jsonOutput {
		return false, nil
	}
	switch mode {
	case "never":
		return false, nil
	case "auto":
		return terminal, nil
	case "always":
		if !terminal {
			return false, errors.New(T("listenbrowse.terminal"))
		}
		return true, nil
	default:
		return false, errors.New(T("listenbrowse.invalid_mode"))
	}
}
