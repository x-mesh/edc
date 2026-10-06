package main

import (
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

func runStress() {
	runtime.GOMAXPROCS(4)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "go-tls-response:"+r.URL.Path)
	}))
	defer server.Close()
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return movingConn{conn}, nil
		},
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	fmt.Println("ready", os.Getpid(), server.URL)
	time.Sleep(3 * time.Second)
	var wait sync.WaitGroup
	errors := make(chan error, 64)
	for worker := 0; worker < 16; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for i := 0; i < 4; i++ {
				path := fmt.Sprintf("/go-tls-probe-%d-%d", worker, i)
				response, err := client.Get(server.URL + path)
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
			}
		}(worker)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("completed 64 HTTPS requests")
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
	time.Sleep(3 * time.Second)
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
	fmt.Println("done")
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "stack" {
		runStack()
	} else {
		runStress()
	}
}
