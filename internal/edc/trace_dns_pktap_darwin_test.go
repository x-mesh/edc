//go:build darwin

package edc

import (
	"fmt"
	"github.com/miekg/dns"
	"net"
	"os"
	"testing"
	"time"
)

func TestDarwinDNSPKTAPLive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("DNS PKTAP capture requires root")
	}
	for _, network := range []string{"udp", "tcp"} {
		t.Run(network, func(t *testing.T) {
			server := &dns.Server{Net: network, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
				time.Sleep(30 * time.Millisecond)
				reply := new(dns.Msg)
				reply.SetReply(r)
				_ = w.WriteMsg(reply)
			})}
			var err error
			if network == "udp" {
				server.PacketConn, err = net.ListenPacket("udp4", "127.0.0.1:53")
			} else {
				server.Listener, err = net.Listen("tcp4", "127.0.0.1:53")
			}
			if err != nil {
				t.Fatalf("bind fixture port 53: %v", err)
			}
			started := make(chan struct{})
			server.NotifyStartedFunc = func() { close(started) }
			serverDone := make(chan error, 1)
			go func() { serverDone <- server.ActivateAndServe() }()
			t.Cleanup(func() { _ = server.Shutdown(); <-serverDone })
			<-started
			previous := captureAttachedHook
			t.Cleanup(func() { captureAttachedHook = previous })
			done := make(chan error, 1)
			captureAttachedHook = func() {
				go func() {
					msg := new(dns.Msg)
					msg.SetQuestion("edc-fixture.invalid.", dns.TypeA)
					client := &dns.Client{Net: network, Timeout: time.Second}
					_, _, err := client.Exchange(msg, "127.0.0.1:53")
					done <- err
				}()
			}
			queries, replies := 0, 0
			summary, err := collectDarwinHTTPEvents(traceScope{protocol: "dns", interfaceName: "lo0"}, 2*time.Second, func(event captureEvent) error {
				if event.Target != "edc-fixture.invalid" {
					return nil
				}
				if event.PID == 0 {
					return fmt.Errorf("DNS process owner is unknown")
				}
				if network == "tcp" && event.Transport != "tcp" {
					return fmt.Errorf("missing TCP transport")
				}
				if event.Event == traceDNSQueryEvent {
					queries++
				} else {
					replies++
					if event.LatencyMS == nil || *event.LatencyMS < 20 {
						return fmt.Errorf("missing DNS latency")
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
			case <-time.After(3 * time.Second):
				t.Fatal("DNS fixture timeout")
			}
			if queries != 1 || replies != 1 || summary.LostEvents != 0 {
				t.Fatalf("queries=%d replies=%d summary=%+v", queries, replies, summary)
			}
		})
	}
}
