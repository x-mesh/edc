//go:build darwin

package edc

import (
	"bytes"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/net/bpf"
)

type darwinHTTPPCAP struct {
	create                      func(string, *byte) uintptr
	wantPKTAP                   func(uintptr, int32) int32
	snaplen, timeout, immediate func(uintptr, int32) int32
	activate, datalink          func(uintptr) int32
	nonblock                    func(uintptr, int32, *byte) int32
	next                        func(uintptr, **darwinPCAPHeader, *unsafe.Pointer) int32
	stats                       func(uintptr, *darwinPCAPStats) int32
	geterr                      func(uintptr) string
	close                       func(uintptr)
	setfilter                   func(uintptr, *darwinPCAPProgram) int32
}
type darwinPCAPHeader struct {
	seconds          int64
	microseconds     int32
	_                [4]byte
	captured, length uint32
	comment          [256]byte
}
type darwinPCAPStats struct{ received, dropped, interfaceDropped uint32 }
type darwinPCAPProgram struct {
	length       uint32
	instructions unsafe.Pointer
}

var loadDarwinHTTPPCAP = sync.OnceValues(func() (*darwinHTTPPCAP, error) {
	library, err := purego.Dlopen("/usr/lib/libpcap.A.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, fmt.Errorf("load libpcap: %w", err)
	}
	api := &darwinHTTPPCAP{}
	names := []string{"pcap_create", "pcap_set_want_pktap", "pcap_set_snaplen", "pcap_set_timeout", "pcap_set_immediate_mode", "pcap_activate", "pcap_datalink", "pcap_setnonblock", "pcap_next_ex", "pcap_stats", "pcap_geterr", "pcap_close", "pcap_setfilter"}
	symbols := make([]uintptr, len(names))
	for i, name := range names {
		symbol, err := purego.Dlsym(library, name)
		if err != nil {
			_ = purego.Dlclose(library)
			return nil, fmt.Errorf("find %s: %w", name, err)
		}
		symbols[i] = symbol
	}
	purego.RegisterFunc(&api.create, symbols[0])
	purego.RegisterFunc(&api.wantPKTAP, symbols[1])
	purego.RegisterFunc(&api.snaplen, symbols[2])
	purego.RegisterFunc(&api.timeout, symbols[3])
	purego.RegisterFunc(&api.immediate, symbols[4])
	purego.RegisterFunc(&api.activate, symbols[5])
	purego.RegisterFunc(&api.datalink, symbols[6])
	purego.RegisterFunc(&api.nonblock, symbols[7])
	purego.RegisterFunc(&api.next, symbols[8])
	purego.RegisterFunc(&api.stats, symbols[9])
	purego.RegisterFunc(&api.geterr, symbols[10])
	purego.RegisterFunc(&api.close, symbols[11])
	purego.RegisterFunc(&api.setfilter, symbols[12])
	return api, nil
})

func openDarwinHTTPPCAP() (*darwinHTTPPCAP, uintptr, error) {
	api, err := loadDarwinHTTPPCAP()
	if err != nil {
		return nil, 0, err
	}
	var buffer [256]byte
	handle := api.create("pktap,all", &buffer[0])
	if handle == 0 {
		return nil, 0, fmt.Errorf("create PKTAP capture: %s", bytes.TrimRight(buffer[:], "\x00"))
	}
	fail := func(message string) (*darwinHTTPPCAP, uintptr, error) {
		detail := api.geterr(handle)
		api.close(handle)
		return nil, 0, fmt.Errorf("%s: %s", message, detail)
	}
	for _, setting := range []struct {
		name  string
		call  func(uintptr, int32) int32
		value int32
	}{
		{"enable PKTAP metadata", api.wantPKTAP, 1}, {"set packet size", api.snaplen, pktapHTTPPacketLimit},
		{"set capture timeout", api.timeout, 100}, {"set immediate capture", api.immediate, 1},
	} {
		if setting.call(handle, setting.value) != 0 {
			return fail(setting.name)
		}
	}
	if api.activate(handle) < 0 {
		return fail("activate PKTAP capture (requires access to /dev/bpf*)")
	}
	if api.datalink(handle) != 149 {
		return fail("PKTAP capture returned an unsupported link type")
	}
	if api.nonblock(handle, 1, &buffer[0]) != 0 {
		return fail("set nonblocking capture")
	}
	return api, handle, nil
}
func (api *darwinHTTPPCAP) setHTTPFilter(handle uintptr, scope traceScope) error {
	instructions, err := pktapHTTPFilter(scope.port)
	if err != nil {
		return err
	}
	raw, err := bpf.Assemble(instructions)
	if err != nil {
		return fmt.Errorf("assemble TCP capture filter: %w", err)
	}
	program := darwinPCAPProgram{length: uint32(len(raw)), instructions: unsafe.Pointer(&raw[0])}
	result := api.setfilter(handle, &program)
	runtime.KeepAlive(raw)
	if result != 0 {
		return fmt.Errorf("install TCP capture filter: %s", api.geterr(handle))
	}
	return nil
}

func httpTracePrerequisites() error {
	api, handle, err := openDarwinHTTPPCAP()
	if err != nil {
		return err
	}
	api.close(handle)
	return nil
}

var captureAttachedHook func()
var captureTrackerHook func(*pktapHTTPTracker)
var capturePacketHook func(pktapTCPPacket, time.Time)
var captureTLSRecordHook func(darwinTLSRecord, uint16, time.Time)

func collectDarwinHTTPEvents(scope traceScope, duration time.Duration, onEvent func(captureEvent) error, stop <-chan struct{}) (captureSummary, error) {
	if scope.protocol == "mysql" && scope.port == 0 {
		scope.port = traceMySQLDefaultPort
	}
	var keylog *tlsKeylog
	if scope.tlsKeylog != "" {
		var err error
		keylog, err = openTLSKeylog(scope.tlsKeylog)
		if err != nil {
			return captureSummary{}, err
		}
		defer keylog.close()
	}

	api, handle, err := openDarwinHTTPPCAP()
	if err != nil {
		return captureSummary{}, err
	}
	defer api.close(handle)
	if err := api.setHTTPFilter(handle, scope); err != nil {
		return captureSummary{}, err
	}
	offset, err := darwinClockOffset()
	if err != nil {
		return captureSummary{}, err
	}
	tracker := newPKTAPHTTPTracker(scope, offset)
	if captureTrackerHook != nil {
		captureTrackerHook(tracker)
	}
	if keylog != nil {
		tracker.tls = newTLSKeylogDecryptor(keylog, tracker)
	}
	var count uint64
	emit := func(events []captureEvent) error {
		for _, event := range events {
			if onEvent != nil {
				if err := onEvent(event); err != nil {
					return err
				}
			}
			count++
		}
		return nil
	}
	tlsCapture, err := newDarwinTLSCapture(scope)
	if err != nil {
		return captureSummary{}, err
	}
	defer tlsCapture.stop()
	emitTLSNotices := func() error {
		for _, notice := range tlsCapture.notices.drain() {
			if onEvent != nil {
				if err := onEvent(captureEvent{TLSNotice: notice}); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for !tlsCapture.initialReady() {
		if err := tlsCapture.poll(tracker, emit, time.Now()); err != nil {
			return captureSummary{}, err
		}
		if err := emitTLSNotices(); err != nil {
			return captureSummary{}, err
		}
		if darwinTraceStopped(stop) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if captureAttachedHook != nil {
		captureAttachedHook()
	}
	var deadline time.Time
	if duration > 0 {
		deadline = time.Now().Add(duration)
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	swept := time.Now()
	for !darwinTraceStopped(stop) && (deadline.IsZero() || time.Now().Before(deadline)) {
		if err := tlsCapture.drain(tracker, emit, time.Now()); err != nil {
			return captureSummary{}, err
		}
		if now := time.Now(); now.Sub(swept) >= 250*time.Millisecond {
			if err := tlsCapture.poll(tracker, emit, now); err != nil {
				return captureSummary{}, err
			}
			if err := emitTLSNotices(); err != nil {
				return captureSummary{}, err
			}
			if tracker.tls != nil {
				if err := emit(tracker.tls.refresh(now)); err != nil {
					return captureSummary{}, err
				}
				if tracker.tls.err != nil {
					return captureSummary{}, tracker.tls.err
				}
			}
			if err := emit(tracker.sweep(now, false)); err != nil {
				return captureSummary{}, err
			}
			swept = now
		}
		var header *darwinPCAPHeader
		var data unsafe.Pointer
		result := api.next(handle, &header, &data)
		switch result {
		case 1:
			if header == nil || data == nil || header.captured > pktapHTTPPacketLimit {
				return captureSummary{}, fmt.Errorf("invalid PKTAP capture record")
			}
			if header.captured < header.length {
				tracker.recordLoss("pktap_truncated", httpPacket{}, fmt.Sprintf("captured=%d original=%d", header.captured, header.length))
				continue
			}
			wall := uint64(header.seconds)*uint64(time.Second) + uint64(header.microseconds)*uint64(time.Microsecond)
			packet, ok := decodeHTTPPKTAP(unsafe.Slice((*byte)(data), int(header.captured)), wall, offset)
			if ok {
				if scope.port != 0 && !pktapHTTPPortMatches(packet, scope.port) {
					continue
				}
				if capturePacketHook != nil {
					capturePacketHook(packet, time.Now())
				}
				if err := emit(tracker.add(packet, time.Now())); err != nil {
					return captureSummary{}, err
				}
				if tracker.tls != nil && tracker.tls.err != nil {
					return captureSummary{}, tracker.tls.err
				}
			}
		case 0:
			select {
			case <-stop:
			case <-tlsCapture.records.ready:
			case <-tick.C:
			}
		default:
			return captureSummary{}, fmt.Errorf("read PKTAP capture: %s", api.geterr(handle))
		}

	}
	if err := tlsCapture.finish(tracker, emit, time.Now()); err != nil {
		return captureSummary{}, err
	}
	if err := emitTLSNotices(); err != nil {
		return captureSummary{}, err
	}
	if tracker.tls != nil {
		if err := emit(tracker.tls.refresh(time.Now())); err != nil {
			return captureSummary{}, err
		}
	}
	if err := emit(tracker.sweep(time.Now(), true)); err != nil {
		return captureSummary{}, err
	}
	var stats darwinPCAPStats
	if api.stats(handle, &stats) != 0 {
		return captureSummary{}, fmt.Errorf("read PKTAP capture statistics: %s", api.geterr(handle))
	}
	unmapped := tlsCapture.unmapped
	notices, noticeOverflow := tlsCapture.notices.summary()
	if noticeOverflow > 0 {
		notices = append(notices, fmt.Sprintf("%d additional TLS notices were omitted", noticeOverflow))
	}
	if tracker.tls != nil {
		if tracker.tls.err != nil {
			return captureSummary{}, tracker.tls.err
		}
		unmapped += tracker.tls.unmapped
		if unmapped != 0 {
			notice := fmt.Sprintf("%d TLS records have no matching key log secrets or complete handshake", unmapped)
			notices = append(notices, notice)
			if onEvent != nil {
				if err := onEvent(captureEvent{TLSNotice: notice}); err != nil {
					return captureSummary{}, err
				}
			}
		}
	}
	return captureSummary{TLSUnmapped: unmapped, TLSNotices: notices, TimestampNS: uint64(time.Now().UnixNano()), Event: "capture_summary", EventCount: count, LostEvents: tracker.lost + uint64(stats.dropped) + uint64(stats.interfaceDropped)}, nil
}
