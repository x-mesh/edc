package edc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
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
	// Command는 container의 첫 process다. --full과 --json에서만 읽는다.
	Command string `json:"command,omitempty"`
}

// attachListenContainers는 Docker Engine이 있으면 publish한 포트의 container를 소켓에 붙인다. Engine socket이 없는 호스트에서는
// 아무것도 하지 않는다. Engine에 닿지 못하면 그 오류를 돌려주고 소켓은 그대로 둔다.
func attachListenContainers(ctx context.Context, sockets []listenSocket, withCommand bool) error {
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
	if !withCommand {
		return nil
	}
	commands := map[string]string{}
	for index := range sockets {
		container := sockets[index].Container
		if container == nil {
			continue
		}
		command, seen := commands[container.ID]
		if !seen {
			// 그 사이에 끝난 container는 첫 process를 모르는 채로 둔다. 명령행의 비밀 값은 listen의 규칙으로 가린다.
			if command, err = listenContainerCommand(ctx, engine, container.ID); err == nil {
				command = strings.Join(listenMaskArgs(strings.Fields(command)), " ")
			}
			commands[container.ID] = command
		}
		container.Command = command
	}
	return nil
}

// listenContainerCommand는 container의 첫 process 명령이다. ps_args를 주지 않는다. OrbStack의 VM은 busybox ps라 edc ctr이 쓰는
// pcpu를 받지 않는다. 열 이름은 ps -ef의 CMD이거나 busybox ps의 COMMAND다.
func listenContainerCommand(ctx context.Context, engine *ctrEngine, id string) (string, error) {
	data, err := engine.get(ctx, "/containers/"+ctrSegment(id)+"/top", ctrJSONLimit)
	if err != nil {
		return "", err
	}
	var top ctrAPITop
	if err := json.Unmarshal(data, &top); err != nil {
		return "", err
	}
	column := slices.IndexFunc(top.Titles, func(title string) bool { return title == "CMD" || title == "COMMAND" })
	if column < 0 || len(top.Processes) == 0 || len(top.Processes[0]) <= column {
		return "", fmt.Errorf("Docker top response has no command")
	}
	return top.Processes[0][column], nil
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
