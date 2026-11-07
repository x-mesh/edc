//go:build darwin

package edc

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const mysqlPKTAPFixtureSQL = "UPDATE edc_fixture SET value = 'mysql-fixture-secret' WHERE id = 7"

func mysqlPKTAPReadFrame(connection net.Conn) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(connection, header); err != nil {
		return nil, err
	}
	size := mysqlLength(header)
	if size > mysqlPacketKeep {
		return nil, fmt.Errorf("MySQL fixture frame exceeds limit")
	}
	frame := append(header, make([]byte, size)...)
	_, err := io.ReadFull(connection, frame[4:])
	return frame, err
}
func mysqlPKTAPFixture(t *testing.T) (uint16, func() error, *atomic.Bool) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	_ = listener.SetDeadline(time.Now().Add(5 * time.Second))
	received := &atomic.Bool{}
	serverDone := make(chan struct{})
	serverResult := make(chan error, 1)
	serve := func() error {
		connection, err := listener.Accept()
		if err != nil {
			return err
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := connection.Write(mysqlGreetingFrame("8.4.11", mysqlTestServerCaps)); err != nil {
			return err
		}
		if _, err := mysqlPKTAPReadFrame(connection); err != nil {
			return err
		}
		if _, err := connection.Write(mysqlOKFrame(2, 0)); err != nil {
			return err
		}
		frame, err := mysqlPKTAPReadFrame(connection)
		if err != nil {
			return err
		}
		if !bytes.Equal(frame, mysqlFrame(0, mysqlQueryPayload(mysqlPKTAPFixtureSQL))) {
			return fmt.Errorf("MySQL fixture received an unexpected query")
		}
		received.Store(true)
		time.Sleep(30 * time.Millisecond)
		_, err = connection.Write(mysqlOKFrame(1, 1))
		return err
	}
	go func() { defer close(serverDone); serverResult <- serve() }()
	t.Cleanup(func() { listener.Close(); <-serverDone })
	run := func() error {
		connection, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
		if err != nil {
			return err
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := mysqlPKTAPReadFrame(connection); err != nil {
			return err
		}
		if _, err := connection.Write(mysqlHandshakeFrame(mysqlTestClientCaps, "fixture", "edc", []byte("fixture-auth"))); err != nil {
			return err
		}
		if _, err := mysqlPKTAPReadFrame(connection); err != nil {
			return err
		}
		if _, err := connection.Write(mysqlFrame(0, mysqlQueryPayload(mysqlPKTAPFixtureSQL))); err != nil {
			return err
		}
		response, err := mysqlPKTAPReadFrame(connection)
		if err != nil {
			return err
		}
		if !bytes.Equal(response, mysqlOKFrame(1, 1)) {
			return fmt.Errorf("MySQL fixture received an unexpected response")
		}
		return <-serverResult
	}
	return uint16(listener.Addr().(*net.TCPAddr).Port), run, received
}
func TestDarwinMySQLWireFixture(t *testing.T) {
	_, run, received := mysqlPKTAPFixture(t)
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if !received.Load() {
		t.Fatal("MySQL wire query was not received")
	}
}
func TestDarwinMySQLPKTAPLive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("live MySQL PKTAP capture requires root")
	}
	port, run, received := mysqlPKTAPFixture(t)
	done := make(chan error, 1)
	previous := captureAttachedHook
	t.Cleanup(func() { captureAttachedHook = previous })
	captureAttachedHook = func() { go func() { done <- run() }() }
	connected, query, response := false, false, false
	summary, err := collectTraceEventsLive(traceScope{protocol: "mysql", side: traceServerSide, port: port}, 2*time.Second, func(event captureEvent) error {
		if event.MySQL == nil {
			return nil
		}
		if event.Side != traceServerSide {
			return fmt.Errorf("MySQL --side server emitted a client event")
		}
		if strings.Contains(event.MySQL.SQL, "mysql-fixture-secret") {
			return fmt.Errorf("MySQL SQL value was not masked")
		}
		connected = connected || event.Event == mysqlEventConnect
		query = query || event.Event == "mysql_query" && strings.Contains(event.MySQL.SQL, "UPDATE edc_fixture")
		if event.Event == mysqlEventOK && event.MySQL.Command == mysqlCommandQuery {
			if event.LatencyMS == nil || *event.LatencyMS < 20 {
				return fmt.Errorf("MySQL response latency missed the server delay")
			}
			response = event.MySQL.AffectedRows != nil && *event.MySQL.AffectedRows == 1
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
		t.Fatal("MySQL fixture did not complete")
	}
	if !received.Load() || !connected || !query || !response || summary.LostEvents != 0 {
		t.Fatalf("wire=%t connect=%t query=%t response=%t summary=%#v", received.Load(), connected, query, response, summary)
	}
	t.Log("captured MySQL connect, query and response with side filtering, masked SQL values and measured latency")
}
