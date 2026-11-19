package edc

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"
)

// listenContainerTimeout은 Engine에 묻는 시간의 상한이다. Engine이 멈춰도 listen 표가 늦게 나오지 않아야 한다.
const listenContainerTimeout = 2 * time.Second

// listenContainer는 소켓의 포트를 publish한 container다. OrbStack Helper, Docker Desktop, docker-proxy는 container 대신 host에서
// 포트를 연다. 그래서 소켓의 주인만으로는 그 포트 뒤의 container를 알 수 없다.
type listenContainer struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Image string `json:"image"`
	// Port와 Proto는 container 안의 포트다.
	Port  int    `json:"port"`
	Proto string `json:"proto"`
	// Command는 container의 첫 process다. 소켓을 대신 연 process보다 이 명령이 실제 서버를 알려 준다.
	Command string `json:"command,omitempty"`
}

// listenContainerCommandShown은 표의 PROCESS 칸에 적는 container 명령의 글자 수다. -v는 그 뒤에 열이 더 있어, 긴 명령 하나가 모든
// 행을 넓히지 않게 자른다. --full의 아랫줄과 JSON에는 명령 전체가 있다.
const listenContainerCommandShown = 40

// listenContainerLabel은 PROCESS 칸의 → 뒤에 적는 container다. 명령을 모르면 이름만 적는다.
func listenContainerLabel(container listenContainer) string {
	if container.Command == "" {
		return container.Name
	}
	return container.Name + ": " + svcTruncate(container.Command, listenContainerCommandShown)
}

// attachListenContainers는 Docker Engine이 있으면 publish한 포트의 container를 소켓에 붙인다. Engine socket이 없는 호스트에서는
// 아무것도 하지 않는다. Engine에 닿지 못하면 그 오류를 돌려주고 소켓은 그대로 둔다. container 명령을 읽지 못하면 붙인 container는
// 두고 첫 오류를 돌려준다.
func attachListenContainers(ctx context.Context, sockets []listenSocket) error {
	path, err := ctrSocketEndpoint()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, listenContainerTimeout)
	defer cancel()
	engine := newCtrEngine(path)
	data, err := engine.get(ctx, "/containers/json", ctrJSONLimit)
	if err != nil {
		return err
	}
	var containers []ctrAPIContainer
	if err := json.Unmarshal(data, &containers); err != nil {
		return err
	}
	matchListenContainers(sockets, containers)
	commands := map[string]string{}
	var first error
	for index := range sockets {
		container := sockets[index].Container
		if container == nil {
			continue
		}
		command, seen := commands[container.ID]
		if !seen {
			args, err := listenContainerCommand(ctx, engine, container.ID)
			switch {
			// 그 사이에 끝난 container는 첫 process를 모르는 채로 둔다.
			case ctrHTTPStatus(err, http.StatusNotFound):
			// 다른 실패는 명령이 없는 container처럼 보이지 않도록 돌려준다.
			case err != nil:
				first = cmp.Or(first, err)
			default:
				command = strings.Join(listenMaskArgs(args), " ")
			}
			commands[container.ID] = command
		}
		container.Command = command
	}
	return first
}

// listenContainerCommand는 container의 첫 process 명령을 인자 단위로 돌려준다. /top의 명령 열은 인자를 공백으로 이은 한 문자열이라
// 공백이 든 비밀 값을 가릴 수 없다. inspect의 Path와 Args는 인자 경계를 그대로 둔다.
func listenContainerCommand(ctx context.Context, engine *ctrEngine, id string) ([]string, error) {
	data, err := engine.get(ctx, "/containers/"+ctrSegment(id)+"/json", ctrJSONLimit)
	if err != nil {
		return nil, err
	}
	var inspect ctrAPIInspect
	if err := json.Unmarshal(data, &inspect); err != nil {
		return nil, err
	}
	if inspect.Path == "" {
		return nil, fmt.Errorf("Docker inspect response has no command")
	}
	return append([]string{inspect.Path}, inspect.Args...), nil
}

// matchListenContainers는 publish한 host 주소, 포트, 프로토콜이 소켓과 같은 container를 붙인다. docker는 0.0.0.0과 ::을 따로
// publish하지만 listen은 둘 다 *로 적는다.
func matchListenContainers(sockets []listenSocket, containers []ctrAPIContainer) {
	type key struct {
		proto, address string
		port           int
	}
	published := map[key]listenContainer{}
	for _, container := range containers {
		name := ""
		if len(container.Names) > 0 {
			name = strings.TrimPrefix(container.Names[0], "/")
		}
		for _, port := range container.Ports {
			// publish하지 않고 EXPOSE만 한 포트는 host에 소켓이 없다.
			if port.PublicPort == 0 {
				continue
			}
			published[key{port.Type, listenAnyAddress(port.IP), port.PublicPort}] = listenContainer{ID: container.ID, Name: name, Image: container.Image, Port: port.PrivatePort, Proto: port.Type}
		}
	}
	for index := range sockets {
		socket := &sockets[index]
		if container, ok := published[key{socket.Proto, listenAnyAddress(socket.Address), socket.Port}]; ok {
			socket.Container = &container
		}
	}
}

// listenAnyAddress는 모든 주소를 뜻하는 표기를 *로 맞춘다.
func listenAnyAddress(address string) string {
	switch address {
	case "", "0.0.0.0", "::", "[::]":
		return "*"
	}
	return address
}
