//go:build darwin && (amd64 || arm64)

// Package fsevents watches a directory tree with macOS FSEvents. It needs no
// file descriptor per watched file, unlike kqueue.
package fsevents

import (
	"errors"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

// Event flags from CoreServices FSEvents.h.
const (
	FlagMustScanSubDirs = 0x00000001
	FlagUserDropped     = 0x00000002
	FlagKernelDropped   = 0x00000004
	FlagRootChanged     = 0x00000020
	FlagItemCreated     = 0x00000100
	FlagItemRemoved     = 0x00000200
	FlagItemRenamed     = 0x00000800
	FlagItemModified    = 0x00001000
	FlagItemIsDir       = 0x00020000
)

const (
	cfStringEncodingUTF8 = 0x08000100
	eventIDSinceNow      = uint64(0xFFFFFFFFFFFFFFFF)
)

var errStreamCreateNull = errors.New("FSEventStreamCreate returned NULL")

// Event is one path of an FSEvents callback. Paths are in Unicode NFC form.
// Flags accumulate: one event can carry flags of earlier changes to the path.
type Event struct {
	Path  string
	Flags uint32
}

type fsEventStreamContext struct {
	version         int
	info            uintptr
	retain          uintptr
	release         uintptr
	copyDescription uintptr
}

// Stream delivers the events under one root until Close.
type Stream struct {
	stream uintptr
	cb     *streamCallback
	pinner runtime.Pinner
	once   sync.Once
	Events <-chan []Event
}

// Start watches root and its whole subtree. latency is how long FSEvents
// collects changes before one callback.
func Start(root string, latency time.Duration) (*Stream, error) {
	cstr := append([]byte(root), 0)
	cfRoot := cfStringCreate(0, unsafe.Pointer(&cstr[0]), cfStringEncodingUTF8)
	if cfRoot == 0 {
		return nil, errors.New("CFStringCreateWithCString returned NULL")
	}
	defer cfRelease(cfRoot)
	values := []uintptr{cfRoot}
	paths := cfArrayCreate(0, unsafe.Pointer(&values[0]), 1, 0)
	if paths == 0 {
		return nil, errors.New("CFArrayCreate returned NULL")
	}
	defer cfRelease(paths)
	cb, err := newStreamCallback()
	if err != nil {
		return nil, err
	}
	stream := &Stream{cb: cb, Events: cb.events}
	stream.pinner.Pin(cb)
	context := fsEventStreamContext{info: uintptr(unsafe.Pointer(cb))}
	stream.stream = fsEventStreamCreate(0, fsEventsCallbackAsmAddr, unsafe.Pointer(&context), paths, eventIDSinceNow, latency.Seconds())
	if stream.stream == 0 {
		cb.close()
		stream.pinner.Unpin()
		return nil, errStreamCreateNull
	}
	fsEventStreamSetDispatchQueue(stream.stream, cb.queue)
	if fsEventStreamStart(stream.stream) == 0 {
		fsEventStreamInvalidate(stream.stream)
		fsEventStreamRelease(stream.stream)
		cb.close()
		stream.pinner.Unpin()
		return nil, errors.New("FSEventStreamStart failed")
	}
	return stream, nil
}

// Close stops the stream and closes Events. It does not wait for a reader.
func (stream *Stream) Close() {
	stream.once.Do(func() {
		close(stream.cb.quit)
		fsEventStreamStop(stream.stream)
		fsEventStreamInvalidate(stream.stream)
		stream.cb.waitDispatchQueue()
		stream.cb.close()
		fsEventStreamRelease(stream.stream)
		stream.pinner.Unpin()
		close(stream.cb.events)
	})
}

// NormalizePath returns path in the NFC form that Event.Path uses.
func NormalizePath(path string) string {
	return normalizeNFC(path)
}

func fsEventsCallback(cb *streamCallback, payload *fsEventsCallbackPayload) {
	defer payload.close()
	if payload == nil || payload.paths == 0 || payload.flags == 0 {
		return
	}
	events := make([]Event, 0, payload.numEvents)
	for index := range payload.numEvents {
		flags := *(*uint32)(unsafe.Add(nil, payload.flags+index*unsafe.Sizeof(uint32(0))))
		path := cfStringToNFC(cfArrayGetValueAtIndex(payload.paths, int(index)))
		if path != "" {
			events = append(events, Event{Path: path, Flags: flags})
		}
	}
	select {
	case cb.events <- events:
	case <-cb.quit:
	}
}
