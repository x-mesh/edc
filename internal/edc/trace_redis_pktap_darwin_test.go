//go:build darwin

package edc

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func redisPKTAPFixture(t *testing.T) (uint16, func() error) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	_ = listener.SetDeadline(time.Now().Add(5 * time.Second))
	request := append(redisPKTAPCommand("AUTH", "fixture-password"), redisPKTAPCommand("SET", "cache", "value-secret")...)
	request = append(request, redisPKTAPCommand("GET", "cache")...)
	request = append(request, redisPKTAPCommand("PING")...)
	response := []byte("+OK\r\n+OK\r\n$12\r\nvalue-secret\r\n+PONG\r\n")
	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, err := listener.Accept()
		if err != nil {
			result <- err
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
		data := make([]byte, len(request))
		if _, err := io.ReadFull(connection, data); err != nil {
			result <- err
			return
		}
		if !bytes.Equal(data, request) {
			result <- fmt.Errorf("Redis fixture command mismatch")
			return
		}
		time.Sleep(30 * time.Millisecond)
		_, err = connection.Write(response)
		result <- err
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	run := func() error {
		connection, err := net.DialTimeout("tcp4", listener.Addr().String(), 3*time.Second)
		if err != nil {
			return err
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := connection.Write(request[:7]); err != nil {
			return err
		}
		time.Sleep(5 * time.Millisecond)
		if _, err := connection.Write(request[7:]); err != nil {
			return err
		}
		reply := make([]byte, len(response))
		if _, err := io.ReadFull(connection, reply); err != nil {
			return err
		}
		if !bytes.Equal(reply, response) {
			return fmt.Errorf("Redis fixture reply mismatch")
		}
		return <-result
	}
	return uint16(listener.Addr().(*net.TCPAddr).Port), run
}
func TestDarwinRedisWireFixture(t *testing.T) {
	_, run := redisPKTAPFixture(t)
	if err := run(); err != nil {
		t.Fatal(err)
	}
}
func TestDarwinRedisPKTAPLive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("Redis PKTAP capture requires root")
	}
	port, run := redisPKTAPFixture(t)
	previous := captureAttachedHook
	t.Cleanup(func() { captureAttachedHook = previous })
	done := make(chan error, 1)
	captureAttachedHook = func() { go func() { done <- run() }() }
	commands, replies := []string{}, []string{}
	summary, err := collectDarwinHTTPEvents(traceScope{protocol: "redis", port: port, side: traceClientSide}, 2*time.Second, func(event captureEvent) error {
		if event.Redis == nil {
			return nil
		}
		if event.Side != traceClientSide {
			return fmt.Errorf("server event passed client-side filter")
		}
		if len(event.Redis.ArgPreview) != 0 || event.Redis.Value != "" || event.Redis.Message != "" {
			return fmt.Errorf("Redis arguments or response values were not masked")
		}
		switch event.Event {
		case redisEventCommand:
			commands = append(commands, event.Redis.Command)
		case redisEventReply:
			replies = append(replies, event.Redis.Command)
			if event.LatencyMS == nil || *event.LatencyMS < 20 {
				return fmt.Errorf("Redis latency missed server delay")
			}
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Redis fixture did not finish")
	}
	want := []string{"AUTH", "SET", "GET", "PING"}
	if fmt.Sprint(commands) != fmt.Sprint(want) || fmt.Sprint(replies) != fmt.Sprint(want) || summary.LostEvents != 0 {
		t.Fatalf("commands=%v replies=%v summary=%#v", commands, replies, summary)
	}
	t.Log("captured Redis pipeline commands and replies with side/port filters, masked values and measured latency")
}
