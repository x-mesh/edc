package edc

import (
	"sort"
	"strconv"
	"strings"
)

// listenSocket은 연결을 기다리는 소켓 하나다. 사람이 묻는 것은 "무엇이 열려 있나"이므로 포트와
// 그 포트를 잡은 프로세스를 함께 담는다.
type listenSocket struct {
	Proto   string
	Address string
	Port    int
	Process string
	PID     string
}

// parseLsofFields는 `lsof -F` 출력을 읽는다. -F는 필드마다 한 줄을 쓰고 앞 글자로 종류를 표시하므로
// 열을 쪼갤 일이 없다. 명령 이름에 공백이 있어도 안전하다.
//
// 블록 구조다. p는 프로세스를 열고 c는 그 이름, f는 소켓 하나를 연다. P는 프로토콜, n은 주소,
// TST=는 TCP 상태다. 읽지 못한 소켓은 버리지 않고 개수로 돌려준다.
func parseLsofFields(text string, udp bool) (sockets []listenSocket, unparsed int) {
	var pid, command, proto, name, state string
	flush := func() {
		if proto == "" && name == "" {
			return
		}
		defer func() { proto, name, state = "", "", "" }()
		if !listenSocketWanted(proto, state, name, udp) {
			return
		}
		address, port, ok := splitHostPort(name)
		if !ok {
			// *:*는 포트를 아직 정하지 않은 소켓이다. 읽지 못한 것이 아니라 보여 줄 포트가 없다.
			if name != "*:*" {
				unparsed++
			}
			return
		}
		sockets = append(sockets, listenSocket{
			Proto: strings.ToLower(proto), Address: address, Port: port,
			Process: command, PID: pid,
		})
	}
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			flush()
			pid, command = line[1:], ""
		case 'c':
			command = line[1:]
		case 'f':
			flush()
		case 'P':
			proto = line[1:]
		case 'n':
			name = line[1:]
		case 'T':
			if value, found := strings.CutPrefix(line[1:], "ST="); found {
				state = value
			}
		}
	}
	flush()
	return sockets, unparsed
}

// parseSSRows는 `ss -Htulnp` 출력을 읽는다. -t와 -u를 함께 주면 맨 앞에 프로토콜 열이 생기므로
// TCP만 볼 때도 같은 형식으로 묻고 여기서 거른다. 형식이 하나면 파서도 하나다.
func parseSSRows(text string, udp bool) (sockets []listenSocket, unparsed int) {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			unparsed++
			continue
		}
		proto, state, local := fields[0], fields[1], fields[4]
		if !listenSocketWanted(proto, state, local, udp) {
			continue
		}
		address, port, ok := splitHostPort(local)
		if !ok {
			if local != "*:*" {
				unparsed++
			}
			continue
		}
		socket := listenSocket{Proto: strings.ToLower(proto), Address: address, Port: port}
		for _, field := range fields[5:] {
			if strings.HasPrefix(field, "users:") {
				socket.Process, socket.PID = parseSSUsers(field)
			}
		}
		sockets = append(sockets, socket)
	}
	return sockets, unparsed
}

// listenSocketWanted는 연결을 기다리는 소켓만 고른다. TCP는 LISTEN 상태로 가려낸다. UDP에는 상태가
// 없어서 lsof가 연결된 흐름까지 함께 돌려주므로 주소로 가려낸다.
func listenSocketWanted(proto, state, name string, udp bool) bool {
	// lsof는 연결된 소켓의 주소를 로컬->원격으로 적는다. QUIC 흐름 같은 것이 여기 걸린다.
	if strings.Contains(name, "->") {
		return false
	}
	switch strings.ToUpper(proto) {
	case "TCP":
		return strings.EqualFold(state, "LISTEN")
	case "UDP":
		return udp
	default:
		return false
	}
}

// parseSSUsers는 `users:(("sshd",pid=21571,fd=3),...)`에서 첫 프로세스만 꺼낸다. 같은 포트를 여러
// 프로세스가 물려받은 경우 첫 번째가 그 포트를 연 쪽이다.
func parseSSUsers(field string) (process, pid string) {
	inner := strings.TrimSuffix(strings.TrimPrefix(field, "users:(("), "))")
	first, _, _ := strings.Cut(inner, "),(")
	parts := strings.Split(first, ",")
	if len(parts) > 0 {
		process = strings.Trim(parts[0], `"`)
	}
	for _, part := range parts {
		if value, found := strings.CutPrefix(part, "pid="); found {
			pid = value
		}
	}
	return process, pid
}

// splitHostPort는 포트를 마지막 콜론 뒤에서 읽는다. IPv6의 [::]와 ss가 붙이는 %iface 표기를 함께 다룬다.
func splitHostPort(value string) (host string, port int, ok bool) {
	index := strings.LastIndex(value, ":")
	if index < 0 {
		return "", 0, false
	}
	host, portText := value[:index], value[index+1:]
	number, err := strconv.Atoi(portText)
	if err != nil {
		return "", 0, false
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if scoped, _, found := strings.Cut(host, "%"); found {
		host = scoped
	}
	if host == "" || host == "*" {
		host = "*"
	}
	return host, number, true
}

// dedupeListenSockets는 같은 소켓을 한 줄로 모은다. 한 포트를 IPv4와 IPv6로 함께 열면 도구가 두 줄을
// 내지만 사람에게는 같은 사실이다.
func dedupeListenSockets(sockets []listenSocket) []listenSocket {
	seen := map[listenSocket]bool{}
	unique := make([]listenSocket, 0, len(sockets))
	for _, socket := range sockets {
		if seen[socket] {
			continue
		}
		seen[socket] = true
		unique = append(unique, socket)
	}
	sort.Slice(unique, func(i, j int) bool {
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
func formatListenTable(sockets []listenSocket) string {
	headers := []string{T("observe.listen.column.port"), T("observe.listen.column.proto"), T("observe.listen.column.address"), T("observe.listen.column.process")}
	rows := make([][]string, 0, len(sockets))
	for _, socket := range sockets {
		process := socket.Process
		if socket.PID != "" {
			process += " (" + socket.PID + ")"
		}
		rows = append(rows, []string{strconv.Itoa(socket.Port), socket.Proto, socket.Address, process})
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
	for _, row := range rows {
		writeRow(row)
	}
	return builder.String()
}
