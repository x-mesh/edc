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
	"strings"
	"sync"
	"time"
)

func requestBody(path string) string { return "h2-request:" + path + ":" + strings.Repeat("q", 65536) }
func responseBody(path string) string {
	return "h2-response:" + path + ":" + strings.Repeat("r", 65536)
}

func main() {
	var serverConnections sync.WaitGroup
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || r.ProtoMajor != 2 || string(body) != requestBody(r.URL.Path) || r.Header.Get("X-Repeated") != "shared-hpack-value" {
			panic("HTTP/2 request mismatch")
		}
		w.Header().Set("X-Repeated", "shared-hpack-value")
		body = []byte(responseBody(r.URL.Path))
		for offset := 0; offset < len(body); offset += 4096 {
			end := min(offset+4096, len(body))
			if _, err := w.Write(body[offset:end]); err != nil {
				panic(err)
			}
			w.(http.Flusher).Flush()
		}
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateNew {
			serverConnections.Add(1)
		}
		if state == http.StateClosed {
			serverConnections.Done()
		}
	}
	server.StartTLS()
	fmt.Println("ready", os.Getpid())
	time.Sleep(3 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	protocols := &http.Protocols{}
	protocols.SetHTTP2(true)
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, Protocols: protocols}
	conn, err := transport.NewClientConn(ctx, "https", strings.TrimPrefix(server.URL, "https://"))
	if err != nil {
		panic(err)
	}
	var wait sync.WaitGroup
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			path := fmt.Sprintf("/h2-stream-%d", i)
			req, err := http.NewRequestWithContext(ctx, "POST", server.URL+path, strings.NewReader(requestBody(path)))
			if err != nil {
				panic(err)
			}
			req.Header.Set("X-Repeated", "shared-hpack-value")
			req.Header.Set("X-Large", strings.Repeat("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ", 1500))
			resp, err := conn.RoundTrip(req)
			if err != nil {
				panic(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.ProtoMajor != 2 || string(body) != responseBody(path) {
				panic("HTTP/2 response mismatch")
			}
		}(i)
	}
	wait.Wait()
	if err := conn.Close(); err != nil {
		panic(err)
	}
	server.Close()
	serverConnections.Wait()
	fmt.Println("done")
}
