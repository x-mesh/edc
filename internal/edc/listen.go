package edc

import (
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
func formatListenTable(sockets []listenSocket, detail bool, note func(string) string) string {
	headers := []string{T("observe.listen.column.port"), T("observe.listen.column.proto"), T("observe.listen.column.address"), T("observe.listen.column.scope"), T("observe.listen.column.process")}
	if detail {
		headers = append(headers, T("observe.listen.column.user"), T("observe.listen.column.fd"), T("observe.listen.column.queue"))
	}
	home, _ := os.UserHomeDir()
	rows := make([][]string, 0, len(sockets))
	commands := make([]string, 0, len(sockets))
	for _, socket := range sockets {
		if note != nil {
			commands = append(commands, listenCommandLine(socket, home))
		}
		process := socket.Process
		if socket.PID != "" {
			process += " (" + socket.PID + ")"
		}
		port := strconv.Itoa(socket.Port)
		if socket.Port == 0 {
			// 유닉스 소켓은 포트가 없다. 빈 칸 대신 없음을 그대로 적는다.
			port = "-"
		}
		row := []string{port, socket.Proto, socket.Address, T("observe.listen.scope." + listenBindingScope(socket.Proto, socket.Address)), process}
		if detail {
			row = append(row, listenCell(socket.User), listenCell(socket.FD), listenQueueCell(socket))
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
		if note != nil && commands[index] != "" {
			builder.WriteString(strings.Repeat(" ", widths[0]+2) + note("↳ "+commands[index]) + "\n")
		}
	}
	return builder.String()
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
	listenSecretFlag    = regexp.MustCompile(`(?i)(^|[-_.])(pass(word|wd)?|secret|token|api[-_]?key|credentials?)s?($|[-_.])`)
	listenURLCredential = regexp.MustCompile(`(://[^/:@\s]*:)[^/@\s]+@`)
)

// listenMaskArgs는 명령줄의 비밀 값을 가린다. listen 출력은 이슈와 history에 그대로 옮겨지는 일이 많아서
// --redact를 주지 않아도 가린다. 이름에 비밀을 뜻하는 낱말이 든 옵션의 값과 URL의 암호가 대상이다.
func listenMaskArgs(args []string) []string {
	masked := slices.Clone(args)
	for index := 1; index < len(masked); index++ {
		arg := listenURLCredential.ReplaceAllString(masked[index], "${1}***@")
		masked[index] = arg
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if !listenSecretFlag.MatchString(name) {
			continue
		}
		if hasValue {
			masked[index] = arg[:strings.IndexByte(arg, '=')+1] + "***"
		} else if index+1 < len(masked) && !strings.HasPrefix(masked[index+1], "-") {
			index++
			masked[index] = "***"
		}
	}
	return masked
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
	if !socket.HasQueue {
		return "-"
	}
	return strconv.Itoa(socket.RecvQ) + "/" + strconv.Itoa(socket.SendQ)
}
