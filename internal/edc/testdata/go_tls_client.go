package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

type movingConn struct{ net.Conn }

func (c movingConn) Read(p []byte) (int, error) {
	runtime.Gosched()
	return c.Conn.Read(p)
}

func (c movingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	time.Sleep(2 * time.Millisecond)
	return n, err
}

type completedConn struct {
	net.Conn
	writes *sync.WaitGroup
}

func (c completedConn) Write(p []byte) (int, error) {
	c.writes.Add(1)
	defer c.writes.Done()
	return c.Conn.Write(p)
}

// waitForTLSWrites는 TLS Write를 실행 중인 goroutine이 없을 때까지 기다린다. Write 도중에 process가 끝나면 edc는 그 Write의
// 반환을 보지 못해 잃은 event로 센다. server가 응답을 보내고 Write에서 돌아오기 전에 client가 응답을 받고 끝날 수 있다.
func waitForTLSWrites() {
	buffer := make([]byte, 1<<20)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if !strings.Contains(string(buffer[:runtime.Stack(buffer, true)]), "crypto/tls.(*Conn).Write(") {
			return
		}
	}
	panic("a TLS Write did not return")
}

func runStress() { runLifecycle(16, 4, 0, false) }

func runLifecycle(workers, rounds int, pause time.Duration, keepAlive bool) {
	runtime.GOMAXPROCS(4)
	address := ""
	if len(os.Args) > 2 {
		address = os.Args[2]
	}
	if address == "" {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "go-tls-response:"+r.URL.Path) }))
		defer server.Close()
		address = server.URL
	}
	var writes sync.WaitGroup
	var descriptorsMu sync.Mutex
	descriptors := map[uintptr]bool{}
	reused := 0
	connections := 0
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialTLSContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			socket, err := conn.(*net.TCPConn).SyscallConn()
			if err != nil {
				conn.Close()
				return nil, err
			}
			if err := socket.Control(func(fd uintptr) {
				descriptorsMu.Lock()
				defer descriptorsMu.Unlock()
				connections++
				if descriptors[fd] {
					reused++
				}
				descriptors[fd] = true
			}); err != nil {
				conn.Close()
				return nil, err
			}
			secure := tls.Client(movingConn{conn}, &tls.Config{InsecureSkipVerify: true})
			if err := secure.HandshakeContext(ctx); err != nil {
				conn.Close()
				return nil, err
			}
			return completedConn{Conn: secure, writes: &writes}, nil
		},
		DisableKeepAlives:   !keepAlive,
		MaxIdleConnsPerHost: workers,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	fmt.Println("ready", os.Getpid(), address)
	waitForStart()
	var wait sync.WaitGroup
	errors := make(chan error, 64)
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for i := 0; i < rounds; i++ {
				path := fmt.Sprintf("/go-tls-probe-%d-%d", worker, i)
				response, err := client.Get(address + path)
				if err != nil {
					errors <- err
					return
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || string(body) != "go-tls-response:"+path {
					errors <- fmt.Errorf("%s: %q, %v", path, body, err)
				}
				runtime.GC()
				time.Sleep(pause)
			}
		}(worker)
	}
	wait.Wait()
	writes.Wait()
	close(errors)
	for err := range errors {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	waitForTLSWrites()
	fmt.Printf("completed %d HTTPS requests fd_reuses=%d fd_connections=%d\n", workers*rounds, reused, connections)
}

type growingConn struct {
	net.Conn
	grow bool
}

//go:noinline
func growRead(conn net.Conn, p []byte, depth int) (int, error) {
	var padding [1024]byte
	padding[depth%len(padding)] = byte(depth)
	var n int
	var err error
	if depth > 0 {
		n, err = growRead(conn, p, depth-1)
	} else {
		runtime.Gosched()
		n, err = conn.Read(p)
	}
	runtime.KeepAlive(&padding)
	return n, err
}
func (c *growingConn) Read(p []byte) (int, error) {
	if c.grow {
		return growRead(c.Conn, p, 128)
	}
	return c.Conn.Read(p)
}

//go:noinline
func captureResponse(conn *tls.Conn) error {
	var buffer [2048]byte
	n, err := conn.Read(buffer[:])
	if n == 0 || !strings.Contains(string(buffer[:n]), "stack-test-response") {
		return fmt.Errorf("read: %d %v", n, err)
	}
	return nil
}

func runStack() {
	runtime.GOMAXPROCS(4)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "stack-test-response") }))
	defer server.Close()
	fmt.Println("ready", os.Getpid())
	waitForStart()
	var wait sync.WaitGroup
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			raw, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "https://"))
			if err != nil {
				panic(err)
			}
			transport := &growingConn{Conn: raw}
			conn := tls.Client(transport, &tls.Config{InsecureSkipVerify: true})
			defer conn.Close()
			if err := conn.Handshake(); err != nil {
				panic(err)
			}
			if _, err := fmt.Fprintf(conn, "GET /stack-%d HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n", i); err != nil {
				panic(err)
			}
			transport.grow = true
			if err := captureResponse(conn); err != nil {
				panic(err)
			}
		}(i)
	}
	wait.Wait()
	waitForTLSWrites()
	fmt.Println("done")
}

func runErrors() {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	raw, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "https://"))
	if err != nil {
		panic(err)
	}
	conn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
	if err := conn.Handshake(); err != nil {
		panic(err)
	}
	fmt.Println("ready", os.Getpid())
	waitForStart()
	if n, err := conn.Write(nil); n != 0 || err != nil {
		panic("empty Write")
	}
	if n, err := conn.Read(nil); n != 0 || err != nil {
		panic("empty Read")
	}
	conn.Close()
	request := []byte("GET /failed HTTP/1.1\r\nHost: localhost\r\n\r\n")
	if n, err := conn.Write(request); n != 0 || err == nil {
		panic("closed Write")
	}
	response := []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
	if n, err := conn.Read(response); n != 0 || err == nil {
		panic("closed Read")
	}
	conn.Close()
	waitForTLSWrites()
	fmt.Println("done")
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "reconnect":
			runLifecycle(1, 128, 0, false)
			return
		case "concurrent":
			runLifecycle(16, 16, 0, false)
			return
		case "keepalive":
			runLifecycle(8, 4, 0, true)
			return
		case "soak":
			runLifecycle(8, 30, 2*time.Second, true)
			return
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "stack" {
		runStack()
	} else if len(os.Args) > 1 && os.Args[1] == "errors" {
		runErrors()
	} else {
		runStress()
	}
}

// waitForStart는 시험이 trace를 모두 붙였다고 stdin에 go 한 줄을 쓸 때까지 기다린다.
func waitForStart() {
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		os.Exit(3)
	}
}
