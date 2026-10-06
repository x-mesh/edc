//go:build linux

package edc

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -go-package edc -type tls_plaintext_record tlsPlaintext trace_tls_plaintext_bpf.c -- -I./bpf -D__TARGET_ARCH_x86

import (
	"bufio"
	"bytes"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type tlsPlaintextRecord struct {
	pid         uint32
	ssl         uint64
	sent        bool
	closed      bool
	payload     []byte
	packet      *httpPacket
	closeSocket uint64
	err         error
}
type tlsPlaintextPIDFilter uint32

func newTLSPlaintextPIDFilter(pid uint32) tlsPlaintextPIDFilter { return tlsPlaintextPIDFilter(pid) }
func (f tlsPlaintextPIDFilter) accept(pid uint32) bool          { return f != 0 && uint32(f) == pid }
func tlsPlaintextExRecord(pid uint32, ssl uint64, sent bool, payload []byte, length int, success bool) (tlsPlaintextRecord, bool) {
	if !success || length <= 0 || length > len(payload) {
		return tlsPlaintextRecord{}, false
	}
	return tlsPlaintextRecord{pid: pid, ssl: ssl, sent: sent, payload: append([]byte(nil), payload[:length]...)}, true
}
func tlsPlaintextPacket(record tlsPlaintextRecord) (httpPacket, bool) {
	if record.closed || len(record.payload) == 0 {
		return httpPacket{}, false
	}
	return httpPacket{pid: record.pid, sent: record.sent, socket: record.ssl, payload: record.payload, captureSource: "openssl_uprobe"}, true
}

type tlsPlaintextCloser struct {
	objects tlsPlaintextObjects
	reader  *ringbuf.Reader
	links   []link.Link
}

func (c *tlsPlaintextCloser) Close() error {
	var e error
	if c.reader != nil {
		e = c.reader.Close()
	}
	for _, l := range c.links {
		e = errors.Join(e, l.Close())
	}
	return errors.Join(e, c.objects.Close())
}
func startTLSPlaintextCollector(scope traceScope, stop <-chan struct{}) (<-chan tlsPlaintextRecord, io.Closer, error) {
	if runtime.GOARCH != "amd64" {
		return nil, nil, fmt.Errorf("OpenSSL plaintext tracing is not supported on %s", runtime.GOARCH)
	}
	paths, err := findTLSLibraries(uint32(scope.pid), nil)
	if err != nil {
		return nil, nil, err
	}
	spec, err := loadTlsPlaintext()
	if err != nil {
		return nil, nil, err
	}
	if err = spec.Variables["target_pid"].Set(uint32(scope.pid)); err != nil {
		return nil, nil, err
	}
	allowAll := scope.pid == 0 && scope.container == nil
	if err = spec.Variables["allow_all"].Set(allowAll); err != nil {
		return nil, nil, err
	}
	c := &tlsPlaintextCloser{}
	fail := func(e error) (<-chan tlsPlaintextRecord, io.Closer, error) { c.Close(); return nil, nil, e }
	if err = spec.LoadAndAssign(&c.objects, nil); err != nil {
		return nil, nil, err
	}
	if scope.pid == 0 && scope.container != nil {
		if scope.container == nil || len(scope.container.cgroups) == 0 {
			return fail(errors.New("OpenSSL plaintext tracing requires a PID or cgroup"))
		}
		one := uint8(1)
		for cgroup := range scope.container.cgroups {
			if err := c.objects.AllowedCgroups.Put(cgroup, one); err != nil {
				return fail(fmt.Errorf("allow cgroup %d: %w", cgroup, err))
			}
		}
	}
	hooks := []struct {
		name  string
		entry bool
		prog  *ebpf.Program
	}{{"SSL_read", true, c.objects.TlsReadEnter}, {"SSL_read", false, c.objects.TlsReadExit}, {"SSL_write", true, c.objects.TlsWriteEnter}, {"SSL_write", false, c.objects.TlsWriteExit}, {"SSL_read_ex", true, c.objects.TlsReadExEnter}, {"SSL_read_ex", false, c.objects.TlsReadExExit}, {"SSL_write_ex", true, c.objects.TlsWriteExEnter}, {"SSL_write_ex", false, c.objects.TlsWriteExExit}, {"SSL_free", true, c.objects.TlsClose}}
	for _, p := range paths {
		x, e := link.OpenExecutable(p)
		if e != nil {
			return fail(e)
		}
		syms, e := elfSymbolSet(p)
		if e != nil {
			return fail(e)
		}
		for _, h := range hooks {
			if !syms[h.name] {
				continue
			}
			if strings.HasSuffix(h.name, "_ex") && syms[strings.TrimSuffix(h.name, "_ex")] {
				continue
			}
			var l link.Link
			if h.entry {
				l, e = x.Uprobe(h.name, h.prog, nil)
			} else {
				l, e = x.Uretprobe(h.name, h.prog, nil)
			}
			if e != nil {
				return fail(fmt.Errorf("attach %s in %s: %w", h.name, p, e))
			}
			c.links = append(c.links, l)
		}
	}
	c.reader, err = ringbuf.NewReader(c.objects.Events)
	if err != nil {
		return fail(err)
	}
	out := make(chan tlsPlaintextRecord)
	go func() {
		defer close(out)
		previous := map[httpStreamKey][]byte{}
		for {
			r, e := c.reader.Read()
			if e != nil {
				if !errors.Is(e, ringbuf.ErrClosed) {
					out <- tlsPlaintextRecord{err: e}
				}
				return
			}
			var raw tlsPlaintextTlsPlaintextRecord
			if e = binary.Read(bytes.NewReader(r.RawSample), binary.LittleEndian, &raw); e != nil {
				out <- tlsPlaintextRecord{err: e}
				continue
			}
			if raw.Kind == 2 {
				out <- tlsPlaintextRecord{closeSocket: raw.Ssl}
				continue
			}
			n := min(int(raw.DataLen), len(raw.Data))
			payload := make([]byte, n)
			for i := range n {
				payload[i] = byte(raw.Data[i])
			}
			stream := httpStreamKey{socket: raw.Ssl, sent: raw.Kind == 1}
			if len(payload) == len(raw.Data) && len(previous[stream]) > 0 && bytes.HasPrefix(payload, previous[stream]) {
				continue
			}
			previous[stream] = bytes.Clone(payload)
			if raw.Kind == 0 && bytes.HasPrefix(payload, http2Preface) {
				continue
			}
			plain := tlsPlaintextRecord{pid: raw.Tgid, ssl: raw.Ssl, sent: raw.Kind == 1, payload: payload}
			packet, _ := tlsPlaintextPacket(plain)
			plain.packet = &packet
			plain.packet.bootTimeNS = raw.TimestampNs
			for _, value := range raw.Comm {
				if value == 0 {
					break
				}
				plain.packet.process += string(byte(value))
			}
			out <- plain
		}
	}()
	if stop != nil {
		go func() { <-stop; _ = c.Close() }()
	}
	return out, c, nil
}
func findTLSLibraries(pid uint32, explicit []string) ([]string, error) {
	seen := map[string]bool{}
	paths := []string{}
	add := func(p string) {
		p = strings.TrimSuffix(p, " (deleted)")
		if !seen[p] {
			if _, e := os.Stat(p); e == nil {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	for _, p := range explicit {
		add(p)
	}
	if pid > 0 {
		f, e := os.Open(filepath.Join("/proc", strconv.Itoa(int(pid)), "maps"))
		if e != nil {
			return nil, e
		}
		s := bufio.NewScanner(f)
		for s.Scan() {
			v := strings.Fields(s.Text())
			if len(v) > 5 && strings.Contains(filepath.Base(v[5]), "libssl.so") {
				add(v[5])
			}
		}
		e = s.Err()
		f.Close()
		if e != nil {
			return nil, e
		}
	}
	if len(paths) == 0 {
		for _, g := range []string{"/usr/lib/*-linux-gnu/libssl.so.*", "/lib/*-linux-gnu/libssl.so.*", "/usr/lib64/libssl.so.*", "/usr/lib/libssl.so.*"} {
			m, _ := filepath.Glob(g)
			for _, p := range m {
				add(p)
			}
		}
	}
	if len(paths) == 0 {
		return nil, errors.New("cannot find libssl")
	}
	return paths, nil
}
func elfSymbolSet(path string) (map[string]bool, error) {
	f, e := elf.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	ss, e := f.DynamicSymbols()
	if e != nil && !errors.Is(e, elf.ErrNoSymbols) {
		return nil, e
	}
	m := map[string]bool{}
	for _, s := range ss {
		m[strings.Split(s.Name, "@")[0]] = true
	}
	return m, nil
}
