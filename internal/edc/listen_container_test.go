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
	var tops []string
	engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/version":
			fmt.Fprint(w, `{"ApiVersion":"1.50","MinAPIVersion":"1.41"}`)
		case strings.HasSuffix(path, "/containers/json"):
			fmt.Fprint(w, `[{"Id":"aaa","Names":["/pg"],"Image":"postgres:16","Ports":[{"IP":"0.0.0.0","PrivatePort":5432,"PublicPort":5432,"Type":"tcp"}]},`+
				`{"Id":"bbb","Names":["/cache"],"Image":"redis:7","Ports":[{"IP":"127.0.0.1","PrivatePort":6379,"PublicPort":16379,"Type":"tcp"}]}]`)
		case strings.HasSuffix(path, "/containers/aaa/top"):
			tops = append(tops, r.URL.RawQuery)
			// docker의 기본 top은 ps -ef라 열 이름이 CMD다.
			fmt.Fprint(w, `{"Titles":["UID","PID","PPID","C","STIME","TTY","TIME","CMD"],"Processes":[["999","10","1","0","10:00","?","00:00:01","postgres -c config_file=/etc/postgresql/postgresql.conf"]]}`)
		case strings.HasSuffix(path, "/containers/bbb/top"):
			tops = append(tops, r.URL.RawQuery)
			// OrbStack의 VM은 busybox ps라 열 이름이 COMMAND다.
			fmt.Fprint(w, `{"Titles":["PID","USER","TIME","COMMAND"],"Processes":[["413","999","0:03","redis-server *:6379 --requirepass hunter2"]]}`)
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
	if sockets[0].Container == nil || sockets[0].Container.Name != "pg" || sockets[0].Container.Command != "" || sockets[2].Container != nil || len(tops) != 0 {
		t.Fatalf("without commands: %+v %+v, tops %v", sockets[0].Container, sockets[2].Container, tops)
	}
	if err := attachListenContainers(context.Background(), sockets, true); err != nil {
		t.Fatal(err)
	}
	// ps_args를 주지 않는다. OrbStack의 busybox ps는 pcpu를 받지 않는다.
	// 명령행의 비밀 값은 listen의 규칙으로 가린다.
	if sockets[0].Container.Command != "postgres -c config_file=/etc/postgresql/postgresql.conf" || sockets[1].Container.Command != "redis-server *:6379 --requirepass ***" || len(tops) != 2 || tops[0] != "" {
		t.Fatalf("with commands: %q %q, tops %q", sockets[0].Container.Command, sockets[1].Container.Command, tops)
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

	// Engine socket이 없는 호스트는 아무것도 하지 않는다.
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")
	plain := []listenSocket{{Proto: "tcp", Address: "*", Port: 5432}}
	if err := attachListenContainers(context.Background(), plain, true); err != nil || plain[0].Container != nil {
		t.Fatalf("no engine: %v %+v", err, plain[0].Container)
	}
}
