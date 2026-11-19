package edc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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
	if err := attachListenContainers(context.Background(), sockets); err != nil {
		t.Fatal(err)
	}
	// 명령행의 비밀 값은 listen의 규칙으로 가린다. 표에도 명령이 나오므로 --full이 아니어도 읽는다.
	if sockets[0].Container == nil || sockets[0].Container.Name != "pg" || sockets[2].Container != nil || len(inspects) != 2 ||
		sockets[0].Container.Command != "docker-entrypoint.sh postgres -c config_file=/etc/postgresql/postgresql.conf" || sockets[1].Container.Command != "redis-server *:6379 --requirepass ***" {
		t.Fatalf("containers: %+v %+v %+v, inspects %q", sockets[0].Container, sockets[1].Container, sockets[2].Container, inspects)
	}
	// 기본 표의 PROCESS 칸은 container 이름과 명령이다. 긴 명령은 자르고, --full의 아랫줄에 포트, 이미지, 명령 전체를 적는다.
	plainTable := formatListenTable(sockets, true, nil)
	for _, want := range []string{"OrbStack Helper (9378) → pg: docker-entrypoint.sh postgres -c config_…", "OrbStack Helper (9378) → cache: redis-server *:6379 --requirepass ***"} {
		if !strings.Contains(plainTable, want) {
			t.Fatalf("table has no %q:\n%s", want, plainTable)
		}
	}
	if strings.Contains(plainTable, "postgres:16") || strings.Contains(plainTable, "↳") {
		t.Fatalf("the table without --full must not show the image or the lines below:\n%s", plainTable)
	}
	table := formatListenTable(sockets, false, func(text string) string { return text })
	for _, want := range []string{"↳ " + T("observe.listen.container_port", 5432, "tcp", "postgres:16") + ": docker-entrypoint.sh postgres -c config_file=/etc/postgresql/postgresql.conf\n",
		"↳ " + T("observe.listen.container_port", 6379, "tcp", "redis:7") + ": redis-server *:6379 --requirepass ***\n", "OrbStack Helper (9378)\n"} {
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
	if err := attachListenContainers(context.Background(), gone); err != nil || gone[0].Container == nil || gone[0].Container.Command != "" {
		t.Fatalf("a container that ended: %v %+v", err, gone[0].Container)
	}
	// 명령을 모르면 이름만 적는다.
	if table := formatListenTable(gone, false, nil); !strings.Contains(table, "→ pg\n") {
		t.Fatalf("a container without a command:\n%s", table)
	}
	busy := []listenSocket{{Proto: "tcp", Address: "*", Port: 5432}}
	inspectStatus = http.StatusInternalServerError
	if err := attachListenContainers(context.Background(), busy); err == nil || busy[0].Container == nil || busy[0].Container.Command != "" {
		t.Fatalf("a failing inspect: %v %+v", err, busy[0].Container)
	}

	// Engine socket이 없는 호스트는 아무것도 하지 않는다.
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")
	plain := []listenSocket{{Proto: "tcp", Address: "*", Port: 5432}}
	if err := attachListenContainers(context.Background(), plain); err != nil || plain[0].Container != nil {
		t.Fatalf("no engine: %v %+v", err, plain[0].Container)
	}
}

// runListen --json은 publish한 포트의 소켓에 container를 담고, Engine에 닿지 못하거나 Engine이 실패하면 그 이유를 container_error에
// 남긴다. 소켓 목록은 이 테스트가 연 TCP 소켓을 포함한 실제 host의 것이다.
func TestRunListenJSONReportsContainersAndEngineErrors(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("listen reads sockets only on Linux and macOS")
	}
	// 소켓을 나열하지 못하는 host만 건너뛴다. 그 밖에 runListen이 실패하면 회귀다.
	if _, err := collectListenSockets(selectedListenFamilies(false, false, false, false)); err != nil {
		t.Skipf("this host cannot list its sockets: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	mode := "ok"
	engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/version":
			fmt.Fprint(w, `{"ApiVersion":"1.50","MinAPIVersion":"1.41"}`)
		case strings.HasSuffix(path, "/containers/json") && mode == "list":
			http.Error(w, `{"message":"engine is restarting"}`, http.StatusInternalServerError)
		case strings.HasSuffix(path, "/containers/json"):
			fmt.Fprintf(w, `[{"Id":"aaa","Names":["/web"],"Image":"nginx:1","Ports":[{"IP":"127.0.0.1","PrivatePort":80,"PublicPort":%d,"Type":"tcp"}]}]`, port)
		case strings.HasSuffix(path, "/containers/aaa/json") && mode == "inspect":
			http.Error(w, `{"message":"inspect failed"}`, http.StatusInternalServerError)
		case strings.HasSuffix(path, "/containers/aaa/json"):
			fmt.Fprint(w, `{"Id":"aaa","Path":"nginx","Args":["-g","daemon off;"]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("DOCKER_CONTEXT", "")
	type socket struct {
		Address   string           `json:"address"`
		Port      int              `json:"port"`
		Container *listenContainer `json:"container"`
	}
	run := func() (string, *listenContainer) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "listen.json")
		if code := runListen([]string{"--json", path}, "test"); code != 0 {
			t.Fatalf("listen exited %d", code)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var report struct {
			Results []struct {
				Metrics struct {
					Sockets        []socket `json:"sockets"`
					ContainerError string   `json:"container_error"`
				} `json:"metrics"`
			} `json:"results"`
		}
		if err := json.Unmarshal(data, &report); err != nil || len(report.Results) != 1 {
			t.Fatalf("report = %s, %v", data, err)
		}
		metrics := report.Results[0].Metrics
		for _, socket := range metrics.Sockets {
			if socket.Address == "127.0.0.1" && socket.Port == port {
				return metrics.ContainerError, socket.Container
			}
		}
		t.Fatalf("the test socket 127.0.0.1:%d is not in the report:\n%s", port, data)
		return "", nil
	}

	t.Setenv("DOCKER_HOST", "unix://"+engine.socket)
	if failure, container := run(); failure != "" || container == nil || container.Name != "web" || container.Image != "nginx:1" || container.Port != 80 || container.Command != "nginx -g daemon off;" {
		t.Fatalf("engine ok: error %q, container %+v", failure, container)
	}
	// inspect가 실패하면 container는 붙이되 명령이 없는 container처럼 보이지 않게 이유를 남긴다.
	mode = "inspect"
	if failure, container := run(); !strings.Contains(failure, "inspect failed") || container == nil || container.Name != "web" || container.Command != "" {
		t.Fatalf("inspect failure: error %q, container %+v", failure, container)
	}
	mode = "list"
	if failure, container := run(); !strings.Contains(failure, "engine is restarting") || container != nil {
		t.Fatalf("list failure: error %q, container %+v", failure, container)
	}
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "nope")
	if failure, container := run(); failure != `docker context "nope" does not exist` || container != nil {
		t.Fatalf("missing context: error %q, container %+v", failure, container)
	}
	// Engine socket이 없는 host는 container 없이 그대로다.
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")
	if failure, container := run(); failure != "" || container != nil {
		t.Fatalf("no engine: error %q, container %+v", failure, container)
	}
}
