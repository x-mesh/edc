//go:build darwin

package edc

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func postgresPKTAPFixture(t *testing.T) (uint16, func() error) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	_ = listener.SetDeadline(time.Now().Add(5 * time.Second))
	request := append(pgStartup("user", "fixture", "database", "edc"), pgQuery("SELECT 'value-secret'")...)
	response := append(pgReady(), append(pgComplete("SELECT 1"), pgReady()...)...)
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
			result <- fmt.Errorf("PostgreSQL fixture command mismatch")
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
			return fmt.Errorf("PostgreSQL fixture reply mismatch")
		}
		return <-result
	}
	return uint16(listener.Addr().(*net.TCPAddr).Port), run
}
func TestDarwinPostgresWireFixture(t *testing.T) {
	_, run := postgresPKTAPFixture(t)
	if err := run(); err != nil {
		t.Fatal(err)
	}
}
func TestDarwinPostgresPKTAPLive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("PostgreSQL PKTAP capture requires root")
	}
	port, run := postgresPKTAPFixture(t)
	previous := captureAttachedHook
	t.Cleanup(func() { captureAttachedHook = previous })
	done := make(chan error, 1)
	captureAttachedHook = func() { go func() { done <- run() }() }
	commands, replies := 0, 0
	summary, err := collectDarwinHTTPEvents(traceScope{protocol: "postgres", port: port, side: traceClientSide}, 2*time.Second, func(event captureEvent) error {
		if event.Postgres == nil {
			return nil
		}
		if event.Side != traceClientSide {
			return fmt.Errorf("wrong side")
		}
		if strings.Contains(event.Postgres.SQL, "value-secret") {
			return fmt.Errorf("unmasked SQL")
		}
		switch event.Event {
		case postgresEventConnect, postgresEventQuery:
			commands++
		case postgresEventResult:
			replies++
			if event.LatencyMS == nil || *event.LatencyMS < 20 {
				return fmt.Errorf("missing latency")
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
		t.Fatal("fixture timeout")
	}
	if commands != 2 || replies != 2 || summary.LostEvents != 0 {
		t.Fatalf("commands=%d replies=%d summary=%+v", commands, replies, summary)
	}
}
