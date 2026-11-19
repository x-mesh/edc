package edc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestMatchListenContainersByAddressPortAndProto(t *testing.T) {
	sockets := []listenSocket{
		{Proto: "tcp", Address: "*", Port: 15432, Process: "OrbStack Helper"},
		{Proto: "tcp", Address: "0.0.0.0", Port: 8181, Process: "docker-proxy"},
		{Proto: "tcp", Address: "::", Port: 8181, Process: "docker-proxy"},
		{Proto: "tcp", Address: "127.0.0.1", Port: 18081, Process: "OrbStack Helper"},
		// OrbStack 자체가 연 포트에는 container가 없다.
		{Proto: "tcp", Address: "127.0.0.1", Port: 32222, Process: "OrbStack Helper"},
		// 같은 포트라도 프로토콜이나 host 주소가 다르면 다른 소켓이다.
		{Proto: "udp", Address: "*", Port: 15432, Process: "mdns"},
		{Proto: "tcp", Address: "192.0.2.5", Port: 18081, Process: "other"},
	}
	containers := []ctrAPIContainer{
		{ID: "b", Names: []string{"/edc-listen-b"}, Image: "redis:7", Ports: []ctrAPIPort{{IP: "0.0.0.0", PrivatePort: 6379, PublicPort: 15432, Type: "tcp"}, {IP: "::", PrivatePort: 6379, PublicPort: 15432, Type: "tcp"}}},
		{ID: "i", Names: []string{"/influxdb3-core"}, Image: "influxdb:3.8.0-core", Ports: []ctrAPIPort{{IP: "0.0.0.0", PrivatePort: 8181, PublicPort: 8181, Type: "tcp"}, {IP: "::", PrivatePort: 8181, PublicPort: 8181, Type: "tcp"}, {PrivatePort: 443, Type: "tcp"}}},
		{ID: "a", Names: []string{"/edc-listen-a"}, Image: "python:3.12-slim", Ports: []ctrAPIPort{{IP: "127.0.0.1", PrivatePort: 8000, PublicPort: 18081, Type: "tcp"}}},
	}
	matchListenContainers(sockets, containers)
	want := []string{"edc-listen-b 6379/tcp", "influxdb3-core 8181/tcp", "influxdb3-core 8181/tcp", "edc-listen-a 8000/tcp", "", "", ""}
	for index, socket := range sockets {
		got := ""
		if socket.Container != nil {
			got = fmt.Sprintf("%s %d/%s", socket.Container.Name, socket.Container.Port, socket.Container.Proto)
		}
		if got != want[index] {
			t.Errorf("socket %d (%s %s:%d) = %q, want %q", index, socket.Proto, socket.Address, socket.Port, got, want[index])
		}
	}
	// 소켓마다 따로 담아서, 한 소켓의 명령을 채워도 다른 container 값이 바뀌지 않는다.
	if sockets[1].Container == sockets[2].Container {
		t.Fatal("sockets must not share one container value")
	}
}

func TestAttachListenContainersReadsTheEngine(t *testing.T) {
	var inspects []string
	// inspectStatus가 0이 아니면 aaa의 inspect가 그 상태로 실패한다.
	inspectStatus := 0
	engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/version":
			fmt.Fprint(w, `{"ApiVersion":"1.50","MinAPIVersion":"1.41"}`)
		case strings.HasSuffix(path, "/containers/json"):
			fmt.Fprint(w, `[{"Id":"aaa","Names":["/pg"],"Image":"postgres:16","Ports":[{"IP":"0.0.0.0","PrivatePort":5432,"PublicPort":5432,"Type":"tcp"}]},`+
				`{"Id":"bbb","Names":["/cache"],"Image":"redis:7","Ports":[{"IP":"127.0.0.1","PrivatePort":6379,"PublicPort":16379,"Type":"tcp"}]}]`)
		case strings.HasSuffix(path, "/containers/aaa/json") && inspectStatus != 0:
			http.Error(w, `{"message":"engine is busy"}`, inspectStatus)
		case strings.HasSuffix(path, "/containers/aaa/json"):
			inspects = append(inspects, "aaa")
			fmt.Fprint(w, `{"Id":"aaa","Path":"docker-entrypoint.sh","Args":["postgres","-c","config_file=/etc/postgresql/postgresql.conf"]}`)
		case strings.HasSuffix(path, "/containers/bbb/json"):
			inspects = append(inspects, "bbb")
			// 공백이 든 비밀 값도 한 인자라 통째로 가린다.
			fmt.Fprint(w, `{"Id":"bbb","Path":"redis-server","Args":["*:6379","--requirepass","hunter2 part"]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "unix://"+engine.socket)
	sockets := []listenSocket{
		{Proto: "tcp", Address: "*", Port: 5432, Process: "OrbStack Helper", PID: "9378"},
		{Proto: "tcp", Address: "127.0.0.1", Port: 16379, Process: "OrbStack Helper", PID: "9378"},
		{Proto: "tcp", Address: "127.0.0.1", Port: 32222, Process: "OrbStack Helper", PID: "9378"},
	}
	if err := attachListenContainers(context.Background(), sockets, false); err != nil {
		t.Fatal(err)
	}
	if sockets[0].Container == nil || sockets[0].Container.Name != "pg" || sockets[0].Container.Command != "" || sockets[2].Container != nil || len(inspects) != 0 {
		t.Fatalf("without commands: %+v %+v, inspects %v", sockets[0].Container, sockets[2].Container, inspects)
	}
	if err := attachListenContainers(context.Background(), sockets, true); err != nil {
		t.Fatal(err)
	}
	// 명령행의 비밀 값은 listen의 규칙으로 가린다.
	if sockets[0].Container.Command != "docker-entrypoint.sh postgres -c config_file=/etc/postgresql/postgresql.conf" || sockets[1].Container.Command != "redis-server *:6379 --requirepass ***" || len(inspects) != 2 {
		t.Fatalf("with commands: %q %q, inspects %q", sockets[0].Container.Command, sockets[1].Container.Command, inspects)
	}
	table := formatListenTable(sockets, false, func(text string) string { return text })
	for _, want := range []string{"OrbStack Helper (9378) → pg (postgres:16)\n", "↳ " + T("observe.listen.container_port", 6379, "tcp") + ": redis-server *:6379 --requirepass ***\n", "OrbStack Helper (9378)\n"} {
		if !strings.Contains(table, want) {
			t.Fatalf("table has no %q:\n%s", want, table)
		}
	}
	data, err := json.Marshal(sockets[1])
	if err != nil || !strings.Contains(string(data), `"container":{"id":"bbb","name":"cache","image":"redis:7","port":6379,"proto":"tcp","command":"redis-server *:6379 --requirepass ***"}`) {
		t.Fatalf("json = %s, %v", data, err)
	}

	if strings.Contains(table, "part") || strings.Contains(string(data), "part") {
		t.Fatalf("a secret with a space leaked:\n%s\n%s", table, data)
	}

	// 그 사이에 끝난 container는 명령 없이 둔다. 다른 실패는 명령 없는 container처럼 보이지 않게 오류로 돌려준다.
	gone := []listenSocket{{Proto: "tcp", Address: "*", Port: 5432}}
	inspectStatus = http.StatusNotFound
	if err := attachListenContainers(context.Background(), gone, true); err != nil || gone[0].Container == nil || gone[0].Container.Command != "" {
		t.Fatalf("a container that ended: %v %+v", err, gone[0].Container)
	}
	busy := []listenSocket{{Proto: "tcp", Address: "*", Port: 5432}}
	inspectStatus = http.StatusInternalServerError
	if err := attachListenContainers(context.Background(), busy, true); err == nil || busy[0].Container == nil || busy[0].Container.Command != "" {
		t.Fatalf("a failing inspect: %v %+v", err, busy[0].Container)
	}

	// Engine socket이 없는 호스트는 아무것도 하지 않는다.
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")
	plain := []listenSocket{{Proto: "tcp", Address: "*", Port: 5432}}
	if err := attachListenContainers(context.Background(), plain, true); err != nil || plain[0].Container != nil {
		t.Fatalf("no engine: %v %+v", err, plain[0].Container)
	}
}
