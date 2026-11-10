package edc

import "strings"

var tunnelLoopbackBinds = map[string]bool{"127.0.0.1": true, "localhost": true}

// parseTunnelLocalForward는 ssh -L 값을 로컬 포트와 목적지로 나눈다. 로컬 포트를 생략하면 auto다.
// edc는 127.0.0.1에만 바인딩하므로 loopback이 아닌 바인딩 주소는 버리지 않고 오류로 알린다.
func parseTunnelLocalForward(name, value string) (tunnelPort, string, error) {
	cut := strings.LastIndex(value, ":")
	if cut < 0 {
		return "", "", tunnelError("tunnel.error.remote_invalid", name, value)
	}
	rest, port := value[:cut], value[cut+1:]
	prefix, host := "", rest
	if strings.HasSuffix(rest, "]") {
		// 목적지가 [::1] 같은 IPv6 주소면 주소 안의 ':'에서 자르지 않는다.
		if open := strings.LastIndex(rest, "["); open >= 0 {
			prefix, host = strings.TrimSuffix(rest[:open], ":"), rest[open:]
		}
	} else if split := strings.LastIndex(rest, ":"); split >= 0 {
		prefix, host = rest[:split], rest[split+1:]
	}
	local := tunnelPort(tunnelAutoPort)
	if prefix != "" {
		bind, number, hasBind := strings.Cut(prefix, ":")
		switch {
		case !hasBind:
			number = bind
		case !tunnelLoopbackBinds[bind]:
			return "", "", tunnelError("tunnel.error.line_bind", bind)
		}
		local = tunnelPort(number)
	}
	return local, host + ":" + port, nil
}

// parseTunnelLine은 폼의 호스트 칸에 적은 한 줄을 정의로 바꾼다. "host host:port"와 붙여 넣은 ssh 명령을 받는다.
// 터널 정의는 호스트와 포워딩 하나만 담으므로, 그 밖의 ssh 옵션은 조용히 버리지 않고 오류로 알린다.
func parseTunnelLine(text string) (tunnelSpec, error) {
	words := strings.Fields(text)
	if len(words) > 0 && words[0] == "ssh" {
		words = words[1:]
	}
	var flag, value string
	var rest []string
	for index := 0; index < len(words); index++ {
		word := words[index]
		switch {
		case word == "-N" || word == "-f" || word == "-fN" || word == "-Nf":
		case strings.HasPrefix(word, "-L") || strings.HasPrefix(word, "-R") || strings.HasPrefix(word, "-D"):
			if flag != "" {
				return tunnelSpec{}, tunnelError("tunnel.error.adhoc_conflict")
			}
			flag, value = word[:2], word[2:]
			if value == "" && index+1 < len(words) {
				index++
				value = words[index]
			}
			if value == "" {
				return tunnelSpec{}, tunnelError("tunnel.error.line_shape")
			}
		case strings.HasPrefix(word, "-"):
			return tunnelSpec{}, tunnelError("tunnel.error.line_option", word)
		default:
			rest = append(rest, word)
		}
	}
	switch {
	case flag != "" && len(rest) == 1:
	case flag == "" && len(rest) == 2:
		// 호스트는 앞과 뒤 어디에 와도 된다. ':'가 있는 쪽이 포워딩이다.
		flag, value = "-L", rest[1]
		if !strings.Contains(value, ":") {
			rest[0], value = value, rest[0]
		}
		if !strings.Contains(value, ":") {
			return tunnelSpec{}, tunnelError("tunnel.error.line_shape")
		}
	default:
		return tunnelSpec{}, tunnelError("tunnel.error.line_shape")
	}
	switch flag {
	case "-R":
		return parseTunnelAdhoc(rest[0], "", value, "")
	case "-D":
		return parseTunnelAdhoc(rest[0], "", "", value)
	default:
		return parseTunnelAdhoc(rest[0], value, "", "")
	}
}
