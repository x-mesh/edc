//go:build darwin

package edc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const darwinRedisTLSReply = "$12\r\nvalue-secret\r\n"

func TestDarwinDBTLSHelper(t *testing.T) {
	protocol := os.Getenv("EDC_DB_TLS_FIXTURE")
	if protocol == "" {
		t.Skip("subprocess fixture")
	}
	fmt.Println("EDC_DB_READY")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	raw, err := net.DialTimeout("tcp4", os.Getenv("EDC_DB_TLS_ADDRESS"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	if protocol == "mysql" {
		if _, err := mysqlPKTAPReadFrame(raw); err != nil {
			t.Fatal(err)
		}
		ssl := make([]byte, 32)
		binary.LittleEndian.PutUint32(ssl, mysqlTestClientCaps|0x800)
		if _, err := raw.Write(mysqlFrame(1, ssl)); err != nil {
			t.Fatal(err)
		}
	}
	// The fixture uses an ephemeral self-signed certificate on loopback.
	conn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
	defer conn.Close()
	if err := conn.Handshake(); err != nil {
		t.Fatal(err)
	}
	if protocol == "redis" {
		request := redisPKTAPCommand("GET", "fixture-secret")
		if _, err := conn.Write(request); err != nil {
			t.Fatal(err)
		}
		reply := make([]byte, len(darwinRedisTLSReply))
		if _, err := io.ReadFull(conn, reply); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(reply, []byte(darwinRedisTLSReply)) {
			t.Fatal("unexpected Redis reply")
		}
	} else {
		login := mysqlHandshakeFrame(mysqlTestClientCaps|0x800, "fixture", "edc", []byte("fixture-password"))
		login[3] = 2
		if _, err := conn.Write(login); err != nil {
			t.Fatal(err)
		}
		if _, err := mysqlPKTAPReadFrame(conn); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(mysqlFrame(0, mysqlQueryPayload(mysqlPKTAPFixtureSQL))); err != nil {
			t.Fatal(err)
		}
		if _, err := mysqlPKTAPReadFrame(conn); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDarwinDBTLSCaptureLive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("DB TLS capture requires root")
	}
	testDarwinDBTLSCapture(t, true)
}

func TestDarwinDBTLSWireFixture(t *testing.T) { testDarwinDBTLSCapture(t, false) }

func testDarwinDBTLSCapture(t *testing.T, capture bool) {
	for _, protocol := range []string{"redis", "mysql"} {
		t.Run(protocol, func(t *testing.T) {
			certServer := httptest.NewTLSServer(nil)
			certificate := certServer.TLS.Certificates[0]
			certServer.Close()
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			serverDone := make(chan error, 1)
			go func() {
				serverDone <- func() error {
					raw, err := listener.Accept()
					if err != nil {
						return err
					}
					defer raw.Close()
					_ = raw.SetDeadline(time.Now().Add(8 * time.Second))
					if protocol == "mysql" {
						if _, err := raw.Write(mysqlGreetingFrame("8.4.11", mysqlTestServerCaps|0x800)); err != nil {
							return err
						}
						if _, err := mysqlPKTAPReadFrame(raw); err != nil {
							return err
						}
					}
					conn := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{certificate}})
					defer conn.Close()
					if err := conn.Handshake(); err != nil {
						return err
					}
					if protocol == "mysql" {
						if _, err := mysqlPKTAPReadFrame(conn); err != nil {
							return err
						}
						if _, err := conn.Write(mysqlOKFrame(3, 0)); err != nil {
							return err
						}
						if _, err := mysqlPKTAPReadFrame(conn); err != nil {
							return err
						}
					} else {
						data := make([]byte, len(redisPKTAPCommand("GET", "fixture-secret")))
						if _, err := io.ReadFull(conn, data); err != nil {
							return err
						}
					}
					time.Sleep(30 * time.Millisecond)
					response := []byte(darwinRedisTLSReply)
					if protocol == "mysql" {
						response = mysqlOKFrame(1, 1)
					}
					if protocol == "redis" {
						if _, err := conn.Write(response[:1]); err != nil {
							return err
						}
						time.Sleep(5 * time.Millisecond)
						response = response[1:]
					}
					_, err = conn.Write(response)
					return err
				}()
			}()
			path, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, path, "-test.run=^TestDarwinDBTLSHelper$", "-test.v")
			child.Env = append(os.Environ(), "EDC_DB_TLS_FIXTURE="+protocol, "EDC_DB_TLS_ADDRESS="+listener.Addr().String())
			input, err := child.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			child.Stderr = &stderr
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = child.Process.Kill() })
			reader := bufio.NewReader(output)
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				if strings.TrimSpace(line) == "EDC_DB_READY" {
					break
				}
			}
			if !capture {
				if _, err := io.WriteString(input, "go\n"); err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, reader)
				if err := child.Wait(); err != nil {
					t.Fatalf("child: %v %s", err, stderr.String())
				}
				if err := <-serverDone; err != nil {
					t.Fatal(err)
				}
				return
			}
			targets, err := findDarwinTLSTargets(ctx, traceTLSMode(path), "")
			if err != nil {
				t.Fatal(err)
			}
			var selected []traceTLSTarget
			for _, target := range targets {
				if target.pid == uint32(child.Process.Pid) {
					selected = append(selected, target)
				}
			}
			if len(selected) != 1 {
				t.Fatalf("fixture TLS discovery=%+v", selected)
			}
			done := make(chan error, 1)
			go func() { _, _ = io.Copy(io.Discard, reader); done <- child.Wait() }()
			previous := captureAttachedHook
			t.Cleanup(func() { captureAttachedHook = previous })
			captureAttachedHook = func() {
				_, err := io.WriteString(input, "go\n")
				if err != nil {
					t.Error(err)
				}
			}
			previousRecords := captureTLSRecordHook
			t.Cleanup(func() { captureTLSRecordHook = previousRecords })
			var records []string
			captureTLSRecordHook = func(record darwinTLSRecord, port uint16, at time.Time) {
				if previousRecords != nil {
					previousRecords(record, port, at)
				}
				if record.pid == uint32(child.Process.Pid) && len(records) < 32 {
					records = append(records, fmt.Sprintf("object=%x sent=%t offset=%d size=%d captured=%d closed=%t", record.object, record.sent, record.offset, record.size, len(record.payload), record.closed))
				}
			}
			requests, replies := 0, 0
			summary, err := collectDarwinHTTPEvents(traceScope{protocol: protocol, port: uint16(listener.Addr().(*net.TCPAddr).Port), side: traceClientSide, tls: traceTLSMode(path), tlsFinder: &traceTLSFinder{targets: selected}}, 3*time.Second, func(event captureEvent) error {
				if !event.TLS {
					return nil
				}
				if event.PID != uint32(child.Process.Pid) {
					return fmt.Errorf("unexpected DB TLS owner")
				}
				if event.Redis != nil {
					if len(event.Redis.ArgPreview) > 0 || event.Redis.Value != "" {
						return fmt.Errorf("Redis TLS value leak")
					}
					if event.Event == redisEventCommand {
						requests++
					} else if event.Event == redisEventReply {
						replies++
					}
				}
				if event.MySQL != nil {
					if strings.Contains(event.MySQL.SQL, "mysql-fixture-secret") {
						return fmt.Errorf("MySQL TLS SQL leak")
					}
					if event.Event == "mysql_query" {
						requests++
					} else if event.Event == mysqlEventOK && event.MySQL.Command == "query" {
						replies++
					}
				}
				if (event.Event == redisEventReply || event.Event == mysqlEventOK && event.MySQL != nil && event.MySQL.Command == "query") && (event.LatencyMS == nil || *event.LatencyMS < 20) {
					return fmt.Errorf("missing DB TLS latency")
				}
				return nil
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatalf("child: %v %s", err, stderr.String())
			}
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
			if requests != 1 || replies != 1 || summary.LostEvents != 0 || summary.TLSUnmapped != 0 {
				t.Fatalf("requests=%d replies=%d summary=%+v records=%v", requests, replies, summary, records)
			}
		})
	}
}
