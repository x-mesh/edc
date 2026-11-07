//go:build darwin

package edc

import (
	"crypto/tls"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

func TestDarwinHTTPPCAPOffline(t *testing.T) {
	api, err := loadDarwinHTTPPCAP()
	if err != nil {
		t.Fatal(err)
	}
	library, err := purego.Dlopen("/usr/lib/libpcap.A.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		t.Fatal(err)
	}
	defer purego.Dlclose(library)
	symbol, err := purego.Dlsym(library, "pcap_open_offline")
	if err != nil {
		t.Fatal(err)
	}
	var open func(string, *byte) uintptr
	purego.RegisterFunc(&open, symbol)
	record := pktapTestRecord(false, false, true)
	data := make([]byte, 40)
	binary.LittleEndian.PutUint32(data, 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(data[4:], 2)
	binary.LittleEndian.PutUint16(data[6:], 4)
	binary.LittleEndian.PutUint32(data[16:], 262144)
	binary.LittleEndian.PutUint32(data[20:], 149)
	binary.LittleEndian.PutUint32(data[24:], 100)
	binary.LittleEndian.PutUint32(data[28:], 123)
	binary.LittleEndian.PutUint32(data[32:], uint32(len(record)))
	binary.LittleEndian.PutUint32(data[36:], uint32(len(record)))
	path := filepath.Join(t.TempDir(), "http.pcap")
	if err := os.WriteFile(path, append(data, record...), 0600); err != nil {
		t.Fatal(err)
	}
	var buffer [256]byte
	handle := open(path, &buffer[0])
	if handle == 0 {
		t.Fatalf("open PCAP: %s", buffer)
	}
	defer api.close(handle)
	var header *darwinPCAPHeader
	var packet unsafe.Pointer
	if result := api.next(handle, &header, &packet); result != 1 {
		t.Fatalf("read PCAP: %d %s", result, api.geterr(handle))
	}
	if header.captured != uint32(len(record)) || header.seconds != 100 || header.microseconds != 123 {
		t.Fatalf("header: %#v", header)
	}
	decoded, ok := decodeHTTPPKTAP(unsafe.Slice((*byte)(packet), int(header.captured)), 100_000_123_000, 0)
	if !ok || decoded.process != "fixture" {
		t.Fatalf("packet: %#v %t", decoded, ok)
	}
}

func TestDarwinHTTPLive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("live PKTAP capture requires root")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				response, err := server.Client().Get(server.URL + "/pktap-fixture")
				if err == nil {
					_, _ = io.Copy(io.Discard, response.Body)
					response.Body.Close()
				}
			}
		}
	}()
	defer func() { close(stop); <-done }()
	var events []captureEvent
	summary, err := collectDarwinHTTPEvents(traceScope{protocol: "http", payload: true, payloadAll: true}, time.Second, func(event captureEvent) error { events = append(events, event); return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	var request, response bool
	for _, event := range events {
		if event.Path == "/pktap-fixture" && event.PID == uint32(os.Getpid()) {
			request = request || event.Method == "GET" && event.Status == 0
			response = response || event.Status == 200
		}
	}
	if !request || !response || summary.EventCount == 0 {
		t.Fatalf("request=%t response=%t summary=%#v events=%#v", request, response, summary, events)
	}
}

func TestDarwinHTTPKeylogOptions(t *testing.T) {
	for _, args := range [][]string{{"tcp", "--tls-keylog", "fixture.keys", "--raw"}, {"http", "--tls", "--tls-keylog", "fixture.keys", "--raw"}} {
		captureTraceStderr(t, func() {
			if code := runTrace(args); code != 2 {
				t.Fatalf("args=%q code=%d", args, code)
			}
		})
	}
	captureTraceStderr(t, func() {
		if code := runTrace([]string{"http", "--tls-keylog", filepath.Join(t.TempDir(), "absent.keys"), "--raw"}); code != 3 {
			t.Fatalf("missing key log code=%d", code)
		}
	})
	options := tcpTraceOptions{tlsKeylog: "fixture.keys"}
	applyTraceHTTPDefaults(&options)
	if options.scope("http").tlsKeylog != "fixture.keys" {
		t.Fatal("key log was not passed to collector")
	}
}

func TestDarwinHTTPSKeylogLive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("live HTTPS PKTAP capture requires root")
	}
	path := filepath.Join(t.TempDir(), "fixture.keys")
	keyfile, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer keyfile.Close()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "ok")
	}))
	server.TLS = &tls.Config{KeyLogWriter: keyfile}
	server.StartTLS()
	defer server.Close()
	client := server.Client()
	client.Timeout = time.Second
	client.Transport.(*http.Transport).DisableKeepAlives = true
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				response, err := client.Get(server.URL + "/https-pktap-fixture")
				if err == nil {
					_, _ = io.Copy(io.Discard, response.Body)
					response.Body.Close()
				}
			}
		}
	}()
	defer func() { close(stop); <-done }()
	var response bool
	summary, err := collectDarwinHTTPEvents(traceScope{protocol: "http", tlsKeylog: path, payload: true, payloadAll: true}, 2*time.Second, func(event captureEvent) error {
		response = response || event.TLS && event.Status == 200 && event.Path == "/https-pktap-fixture"
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !response || summary.EventCount == 0 {
		t.Fatalf("decrypted response=%t summary=%#v", response, summary)
	}
}

func TestDarwinHTTPPCAPFilter(t *testing.T) {
	api, err := loadDarwinHTTPPCAP()
	if err != nil {
		t.Fatal(err)
	}
	library, err := purego.Dlopen("/usr/lib/libpcap.A.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		t.Fatal(err)
	}
	defer purego.Dlclose(library)
	symbol, err := purego.Dlsym(library, "pcap_open_offline")
	if err != nil {
		t.Fatal(err)
	}
	var open func(string, *byte) uintptr
	purego.RegisterFunc(&open, symbol)
	for _, test := range []struct {
		name string
		port uint16
		want []uint64
	}{{"all-tcp", 0, []uint64{1, 2, 4, 5}}, {"selected-port", 8080, []uint64{1, 4, 5}}} {
		t.Run(test.name, func(t *testing.T) {
			data := make([]byte, 24)
			binary.LittleEndian.PutUint32(data, 0xa1b2c3d4)
			binary.LittleEndian.PutUint16(data[4:], 2)
			binary.LittleEndian.PutUint16(data[6:], 4)
			binary.LittleEndian.PutUint32(data[16:], 262144)
			binary.LittleEndian.PutUint32(data[20:], 149)
			records := [][]byte{pktapTestRecord(false, false, true), pktapTestRecord(false, false, true), pktapTestRecord(false, false, true), pktapTestRecord(false, true, true), pktapTestRecord(false, true, true)}
			for i, record := range records {
				length := int(binary.LittleEndian.Uint32(record))
				family := binary.LittleEndian.Uint32(record[40:])
				binary.LittleEndian.PutUint32(record[length:], family)
				ip := record[length+4:]
				if i == 1 {
					binary.BigEndian.PutUint16(ip[22:], 9090)
				}
				if i == 2 {
					ip[9] = 17
				}
				if i == 4 {
					tcp := append([]byte{}, ip[40:]...)
					ip = append(ip[:40], make([]byte, 8)...)
					ip = append(ip, tcp...)
					ip[6] = 60
					ip[40] = 6
					binary.BigEndian.PutUint16(ip[4:], uint16(len(ip)-40))
					record = append(record[:length+4], ip...)
				}
				header := make([]byte, 16)
				binary.LittleEndian.PutUint32(header, uint32(i+1))
				binary.LittleEndian.PutUint32(header[8:], uint32(len(record)))
				binary.LittleEndian.PutUint32(header[12:], uint32(len(record)))
				data = append(data, header...)
				data = append(data, record...)
			}
			path := filepath.Join(t.TempDir(), "filtered.pcap")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			var buffer [256]byte
			handle := open(path, &buffer[0])
			if handle == 0 {
				t.Fatalf("open capture: %s", buffer)
			}
			defer api.close(handle)
			if err := api.setHTTPFilter(handle, traceScope{port: test.port}); err != nil {
				t.Fatal(err)
			}
			var got []uint64
			for {
				var header *darwinPCAPHeader
				var packet unsafe.Pointer
				result := api.next(handle, &header, &packet)
				if result == -2 {
					break
				}
				if result != 1 {
					t.Fatalf("read filtered capture: %d %s", result, api.geterr(handle))
				}
				got = append(got, uint64(header.seconds))
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("filtered packets=%v want %v", got, test.want)
			}
		})
	}
}

func TestDarwinPCAPTimevalPadding(t *testing.T) {
	storage := make([]byte, unsafe.Sizeof(darwinPCAPHeader{}))
	binary.LittleEndian.PutUint64(storage, 2_000_000_000)
	binary.LittleEndian.PutUint32(storage[8:], 123)
	binary.LittleEndian.PutUint32(storage[12:], 0xdeadbeef)
	header := (*darwinPCAPHeader)(unsafe.Pointer(&storage[0]))
	if header.seconds != 2_000_000_000 || header.microseconds != 123 {
		t.Fatalf("timeval seconds=%d microseconds=%d", header.seconds, header.microseconds)
	}
}
