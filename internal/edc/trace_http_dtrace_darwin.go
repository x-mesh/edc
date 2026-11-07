//go:build darwin

package edc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const darwinTLSDiagnosticLimit = 8 << 10

type darwinTLSDiagnostics struct {
	sync.Mutex
	data      []byte
	dropped   bool
	tail      []byte
	truncated bool
}

func (d *darwinTLSDiagnostics) Write(data []byte) (int, error) {
	d.Lock()
	defer d.Unlock()
	remaining := darwinTLSDiagnosticLimit - len(d.data)
	d.data = append(d.data, data[:min(len(data), remaining)]...)
	d.truncated = d.truncated || len(data) > remaining
	scan := append(d.tail, data...)
	lower := bytes.ToLower(scan)
	d.dropped = d.dropped || bytes.Contains(lower, []byte(" drops")) || bytes.Contains(lower, []byte(" drop on"))
	d.tail = bytes.Clone(scan[max(0, len(scan)-128):])
	return len(data), nil
}
func (d *darwinTLSDiagnostics) snapshot() (string, bool) {
	d.Lock()
	defer d.Unlock()
	message := strings.TrimSpace(string(bytes.Clone(d.data)))
	if d.truncated {
		message += " (DTrace stderr truncated)"
	}
	return message, d.dropped
}

type darwinTLSWorker struct {
	command               *exec.Cmd
	diagnostics           darwinTLSDiagnostics
	done                  chan struct{}
	ready                 atomic.Bool
	started               time.Time
	err                   error
	announced             bool
	stopRequested, forced bool
}

func (worker *darwinTLSWorker) enqueue(records *darwinTLSRecordQueue, record darwinTLSRecord) bool {
	if err := records.push(record); err != nil {
		worker.err = err
		return false
	}
	return true
}

func startDarwinTLSWorker(targets []traceTLSTarget, records *darwinTLSRecordQueue) (*darwinTLSWorker, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("macOS TLS worker has no targets")
	}
	pid := targets[0].pid
	for _, target := range targets {
		if target.pid != pid {
			return nil, fmt.Errorf("macOS TLS worker contains multiple process IDs")
		}
	}
	script, err := darwinTLSScript(targets)
	if err != nil {
		return nil, err
	}
	command := exec.Command("/usr/sbin/dtrace", "-q", "-p", strconv.FormatUint(uint64(pid), 10), "-s", "/dev/stdin")
	command.Stdin = strings.NewReader(script)
	worker := &darwinTLSWorker{command: command, done: make(chan struct{}), started: time.Now()}
	command.Stderr = &worker.diagnostics
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("start macOS TLS probes for PID %d: %w", pid, err)
	}
	go func() {
		defer close(worker.done)
		scanner := bufio.NewScanner(stdout)
		decoder := darwinTLSDecoder{}
		order := newDarwinTLSGoOrder()
		addresses := newDarwinTLSAddressCache(pid)
		trackAddresses := false
		for _, target := range targets {
			if target.goMachine != 0 || slices.Contains(target.symbols, "SSLRead") {
				trackAddresses = true
			}
		}
		send := func(record darwinTLSRecord) bool {
			if record.pid != pid {
				worker.err = fmt.Errorf("TLS record belongs to PID %d, expected %d", record.pid, pid)
				return false
			}
			if record.closed {
				addresses.forget(record.object)
			}
			if targets[0].process != "" {
				record.process = targets[0].process
			}
			return worker.enqueue(records, record)
		}

		for scanner.Scan() {
			line := scanner.Text()
			if line == "EDC_READY" {
				worker.ready.Store(true)
				continue
			}
			record, err := decoder.line(line)
			if err != nil {
				worker.err = err
				_ = command.Process.Kill()
				break
			}
			if record != nil {
				if record.pid != pid {
					worker.err = fmt.Errorf("TLS record belongs to PID %d, expected %d", record.pid, pid)
					_ = command.Process.Kill()
					break
				}
				if record.fdMeta != 0 {
					if err := addresses.metadata(*record); err != nil {
						worker.err = err
						_ = command.Process.Kill()
						break
					}
					continue
				}
				// Legacy OpenSSL probes do not emit FD generations, so retain their live address lookup.
				if trackAddresses {
					if err := addresses.decorate(record); err != nil {
						worker.err = err
						_ = command.Process.Kill()
						break
					}
				}
				output, err := order.add(*record)
				if err != nil {
					worker.err = err
					_ = command.Process.Kill()
					break
				}
				for _, ready := range output {
					if !send(ready) {
						break
					}
				}
				if worker.err != nil {
					_ = command.Process.Kill()
					break
				}
			}

		}
		if err := scanner.Err(); err != nil && worker.err == nil {
			worker.err = err
			_ = command.Process.Kill()
		}
		if decoder.record != nil && worker.err == nil {
			worker.err = fmt.Errorf("DTrace ended during a TLS record")
		}
		if worker.err == nil {
			output, err := order.finish()
			if err != nil {
				worker.err = err
			}
			for _, record := range output {
				if !send(record) {
					break
				}
			}
		}
		if err := command.Wait(); err != nil && worker.err == nil {
			worker.err = err
		}
	}()
	return worker, nil
}

type darwinTLSDiscovery struct {
	targets []traceTLSTarget
	err     error
}
type darwinTLSCapture struct {
	scope     traceScope
	ctx       context.Context
	cancel    context.CancelFunc
	records   *darwinTLSRecordQueue
	workers   map[uint32]*darwinTLSWorker
	attempted map[uint32]bool
	discovery <-chan darwinTLSDiscovery
	nextScan  time.Time
	notices   *traceTLSNoticeBuffer
	unmapped  uint64
	stopped   bool
}

func newDarwinTLSCapture(scope traceScope) (*darwinTLSCapture, error) {
	ctx, cancel := context.WithCancel(context.Background())
	capture := &darwinTLSCapture{scope: scope, ctx: ctx, cancel: cancel, records: newDarwinTLSRecordQueue(), workers: map[uint32]*darwinTLSWorker{}, attempted: map[uint32]bool{}, notices: newTraceTLSNoticeBuffer(), nextScan: time.Now().Add(2 * time.Second)}
	if scope.tls == "" || scope.tlsKeylog != "" {
		return capture, nil
	}
	finder := scope.tlsFinder
	if finder == nil {
		var notices []string
		var err error
		finder, notices, _, err = resolveTraceHTTPTLSTargets(scope.tls, scope.implicitTLS, scope.process)
		if err != nil {
			cancel()
			return nil, err
		}
		for _, notice := range notices {
			capture.notices.offer(notice)
		}
	}
	if err := capture.attach(finder.targets); err != nil {
		capture.stop()
		return nil, err
	}
	return capture, nil
}
func (c *darwinTLSCapture) attach(targets []traceTLSTarget) error {
	groups := map[uint32][]traceTLSTarget{}
	for _, target := range targets {
		if target.darwinNotice != "" {
			c.notices.offer(target.darwinNotice)
		}
		groups[target.pid] = append(groups[target.pid], target)
	}
	pids := make([]uint32, 0, len(groups))
	for pid := range groups {
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	for _, pid := range pids {
		if c.attempted[pid] {
			continue
		}
		if len(c.workers) >= darwinTLSTargetLimit {
			return fmt.Errorf("macOS TLS exceeds %d active processes; narrow --process or --tls=<path>", darwinTLSTargetLimit)
		}
		c.attempted[pid] = true
		worker, err := startDarwinTLSWorker(groups[pid], c.records)
		if err != nil {
			if !c.scope.implicitTLS {
				return err
			}
			c.notices.offer(err.Error())
			continue
		}
		c.workers[pid] = worker
	}
	return nil
}
func (c *darwinTLSCapture) initialReady() bool {
	for _, worker := range c.workers {
		if !worker.ready.Load() {
			return false
		}
	}
	return true
}
func (c *darwinTLSCapture) poll(tracker *pktapHTTPTracker, emit func([]captureEvent) error, now time.Time) error {
	if err := c.drain(tracker, emit, now); err != nil {
		return err
	}
	for pid, worker := range c.workers {
		diagnostic, dropped := worker.diagnostics.snapshot()
		if dropped {
			return fmt.Errorf("DTrace dropped TLS records for PID %d: %s", pid, diagnostic)
		}
		select {
		case <-worker.done:
			delete(c.workers, pid)
			for queued := c.records.len(); queued > 0; queued -= 64 {
				if err := c.drain(tracker, emit, now); err != nil {
					return err
				}
			}
			if err := emit(tracker.finishDarwinTLSPID(pid, now)); err != nil {
				return err
			}
			problem := worker.err
			if !worker.ready.Load() && problem == nil {
				problem = fmt.Errorf("DTrace exited before TLS probes became ready")
			}
			if problem != nil {
				message := fmt.Sprintf("macOS TLS capture for PID %d failed: %v: %s", pid, problem, diagnostic)
				if !c.scope.implicitTLS || worker.ready.Load() {
					return fmt.Errorf("%s", message)
				}
				// Implicit discovery keeps plain HTTP available when a target rejects attachment.
				c.notices.offer(message)
			}
		default:
			if !worker.ready.Load() && now.Sub(worker.started) > 5*time.Second {
				return fmt.Errorf("TLS probes for PID %d did not become ready within 5s: %s", pid, diagnostic)
			}
			if worker.ready.Load() && !worker.announced {
				worker.announced = true
				c.notices.offer(fmt.Sprintf("macOS TLS probes attached to PID %d", pid))
			}
		}
	}
	if c.scope.tls == traceTLSAuto && c.scope.tlsKeylog == "" && !c.stopped && c.discovery == nil && !now.Before(c.nextScan) {
		results := make(chan darwinTLSDiscovery, 1)
		c.discovery = results
		go func() {
			targets, err := findDarwinTLSTargets(c.ctx, traceTLSAuto, c.scope.process)
			results <- darwinTLSDiscovery{targets, err}
		}()
		c.nextScan = now.Add(2 * time.Second)
	}
	if c.discovery != nil {
		select {
		case result := <-c.discovery:
			c.discovery = nil
			if result.err != nil {
				if !c.scope.implicitTLS {
					return result.err
				}
				c.notices.offer(result.err.Error())
			} else {
				present := map[uint32]bool{}
				for _, target := range result.targets {
					present[target.pid] = true
				}
				for pid := range c.attempted {
					if !present[pid] {
						delete(c.attempted, pid)
					}
				}
				if err := c.attach(result.targets); err != nil {
					return err
				}
			}
		default:
		}
	}
	return nil
}
func (c *darwinTLSCapture) drain(tracker *pktapHTTPTracker, emit func([]captureEvent) error, now time.Time) error {
	for i := 0; i < 64; i++ {
		record, ok := c.records.pop()
		if !ok {
			return nil
		}
		if captureTLSRecordHook != nil {
			captureTLSRecordHook(record, c.scope.port, now)
		}
		events, unmapped := tracker.addDarwinTLS(record, c.scope.port, now)
		if unmapped {
			c.unmapped++
		}
		if err := emit(events); err != nil {
			return err
		}
		if record.size > uint64(len(record.payload)) {
			c.notices.offer("A TLS call exceeded 1 MiB; capture stopped for that TLS session")
		}
	}
	return nil
}
func (c *darwinTLSCapture) stop() {
	if c.stopped {
		return
	}
	c.stopped = true
	c.cancel()
	for _, worker := range c.workers {
		select {
		case <-worker.done:
		default:
			worker.stopRequested = worker.command.Process.Signal(os.Interrupt) == nil
		}
	}
	deadline := time.Now().Add(time.Second)
	for _, worker := range c.workers {
		delay := time.Until(deadline)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-worker.done:
		case <-timer.C:
			worker.forced = worker.command.Process.Kill() == nil
			<-worker.done
		}
		timer.Stop()
	}
}
func (c *darwinTLSCapture) finish(tracker *pktapHTTPTracker, emit func([]captureEvent) error, now time.Time) error {
	if err := c.poll(tracker, emit, now); err != nil {
		return err
	}
	c.stop()
	for queued := c.records.len(); queued > 0; queued -= 64 {
		if err := c.drain(tracker, emit, now); err != nil {
			return err
		}
	}
	for pid, worker := range c.workers {
		diagnostic, dropped := worker.diagnostics.snapshot()
		if dropped {
			return fmt.Errorf("DTrace dropped TLS records for PID %d: %s", pid, diagnostic)
		}
		if !worker.ready.Load() {
			problem := fmt.Sprintf("TLS probes for PID %d did not attach: %s", pid, diagnostic)
			if !c.scope.implicitTLS {
				return fmt.Errorf("%s", problem)
			}
			c.notices.offer(problem)
		}
		if worker.err != nil && worker.ready.Load() {
			exit, ok := worker.err.(*exec.ExitError)
			expected := false
			if ok {
				if status, valid := exit.Sys().(syscall.WaitStatus); valid {
					expected = worker.stopRequested && status.Signal() == syscall.SIGINT || worker.forced && status.Signal() == syscall.SIGKILL
				}
			}
			if !expected {
				return fmt.Errorf("macOS TLS capture for PID %d failed: %v: %s", pid, worker.err, diagnostic)
			}
		}
		if err := emit(tracker.finishDarwinTLSPID(pid, now)); err != nil {
			return err
		}
	}
	return nil
}

// The foreign address offsets follow struct in_sockinfo in the macOS SDK.
const darwinTLSForeignAddress = 32
const darwinTLSForeignAddress4 = 44

func darwinTLSAddresses(pid uint32, fd int32) (string, string) {
	if fd < 0 {
		return "", ""
	}
	info := make([]byte, darwinSocketFDInfoSize)
	if !darwinReadSocketInfo(int32(pid), fd, info) {
		return "", ""
	}
	return darwinTLSAddressesFromInfo(info)
}
func darwinTLSAddressesFromInfo(info []byte) (string, string) {
	if len(info) < darwinSocketFDInfoSize || binary.LittleEndian.Uint32(info[darwinSoiKind:]) != darwinSocketKindTCP {
		return "", ""
	}
	proto := info[darwinSoiProto:]
	var local, remote netip.Addr
	if proto[darwinInVFlag]&darwinInIPv4 != 0 {
		local = netip.AddrFrom4([4]byte(proto[darwinInLAddr4 : darwinInLAddr4+4]))
		remote = netip.AddrFrom4([4]byte(proto[darwinTLSForeignAddress4 : darwinTLSForeignAddress4+4]))
	} else {
		local = netip.AddrFrom16([16]byte(proto[darwinInLAddr : darwinInLAddr+16]))
		remote = netip.AddrFrom16([16]byte(proto[darwinTLSForeignAddress : darwinTLSForeignAddress+16]))
	}
	return netip.AddrPortFrom(local.Unmap(), binary.BigEndian.Uint16(proto[darwinInLPort:])).String(), netip.AddrPortFrom(remote.Unmap(), binary.BigEndian.Uint16(proto[darwinInFPort:])).String()
}
func (t *pktapHTTPTracker) addDarwinTLS(record darwinTLSRecord, port uint16, now time.Time) ([]captureEvent, bool) {
	source, destination := record.source, record.destination
	if source == "" && destination == "" {
		source, destination = darwinTLSAddresses(record.pid, record.fd)
	}
	packet := pktapTCPPacket{httpPacket: httpPacket{bootTimeNS: uint64(int64(record.wallNS) - t.clockOffset), pid: record.pid, process: record.process, sent: record.sent, source: source, destination: destination, payload: record.payload, decrypted: true}, sequence: record.offset, tlsObject: record.object}
	if record.closed {
		packet.flags = 4
	}
	if !record.closed && port != 0 && !pktapHTTPPortMatches(packet, port) {
		return nil, true
	}
	events := t.add(packet, now)
	if record.size > uint64(len(record.payload)) {
		t.recordLoss("tls_call_limit", packet.httpPacket, fmt.Sprintf("captured=%d original=%d", len(record.payload), record.size))
		if flow := t.flows[pktapHTTPKey{pid: record.pid, tlsObject: record.object}]; flow != nil {
			for i := range flow.directions {
				direction := &flow.directions[i]
				t.buffered -= len(direction.buffer) + direction.waitingBytes
				direction.buffer = nil
				direction.waiting = nil
				direction.waitingBytes = 0
				direction.blocked = true
			}
			if t.messages != nil {
				events = append(events, t.messages.forget(flow.id)...)
			}
			events = append(events, t.tracker.forget(flow.id)...)
		}
	}
	return events, false
}
func (t *pktapHTTPTracker) finishDarwinTLSPID(pid uint32, now time.Time) []captureEvent {
	var events []captureEvent
	for key, flow := range t.flows {
		if key.pid == pid && key.tlsObject != 0 {
			events = append(events, t.closeFlow(key, flow, now)...)
		}
	}
	return events
}
