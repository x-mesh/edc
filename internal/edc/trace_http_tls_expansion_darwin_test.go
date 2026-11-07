//go:build darwin

package edc

import (
	"bufio"
	"bytes"
	"context"
	"debug/buildinfo"
	"debug/dwarf"
	"debug/elf"
	"debug/gosym"
	"debug/macho"
	_ "embed"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ebitengine/purego"
)

//go:embed testdata/darwin_securetransport_client.c
var darwinSecureTransportFixture string

//go:embed testdata/go_tls_client.go
var darwinGoTLSFixture string

const darwinSecureTransportProbe = `
#pragma D option quiet
#pragma D option strsize=256
#pragma D option bufsize=2m

BEGIN { printf("EDC_PROBES_READY\n"); }
pid$target::SSLRead:entry,
pid$target::SSLWrite:entry
{
 self->edc_buffer=arg1;
 self->edc_requested=arg2;
 self->edc_processed=arg3;
}
pid$target::SSLRead:return,
pid$target::SSLWrite:return
/self->edc_processed/
{
 this->size=*(userland uint64_t *)self->edc_processed;
}
pid$target::SSLRead:return,
pid$target::SSLWrite:return
/self->edc_processed && this->size > 0 && this->size <= self->edc_requested/
{
 printf("EDC_SECURETRANSPORT %s %d %llu %s\n",probefunc,(int)arg1,this->size,
  copyinstr(self->edc_buffer,this->size < 128 ? this->size : 128));
}
pid$target::SSLRead:return,
pid$target::SSLWrite:return
{ self->edc_buffer=0; self->edc_requested=0; self->edc_processed=0; }
ERROR { printf("EDC_PROBE_FAULT\n"); }
`

func buildDarwinTLSExpansionFixture(t *testing.T, goTLS bool, arch string) string {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "client")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var command *exec.Cmd
	if goTLS {
		source := filepath.Join(directory, "client.go")
		if err := os.WriteFile(source, []byte(darwinGoTLSFixture), 0600); err != nil {
			t.Fatal(err)
		}
		command = exec.CommandContext(ctx, "go", "build", "-o", path, source)
		command.Env = append(os.Environ(), "GOOS=darwin", "GOARCH="+arch, "CGO_ENABLED=0")
	} else {
		source := filepath.Join(directory, "client.c")
		if err := os.WriteFile(source, []byte(darwinSecureTransportFixture), 0600); err != nil {
			t.Fatal(err)
		}
		command = exec.CommandContext(ctx, "/usr/bin/clang", "-O0", "-Wno-deprecated-declarations", source, "-framework", "Security", "-o", path)
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build TLS expansion fixture: %v\n%s", err, output)
	}
	return path
}

func darwinGoTLSProbeScript(path string) (string, error) {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read probe Go build info: %w", err)
	}
	if !traceTLSGoVersions[info.GoVersion] {
		return "", fmt.Errorf("probe requires a validated Go ABI: %s", info.GoVersion)
	}
	file, err := macho.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	text, pcln := file.Section("__text"), file.Section("__gopclntab")
	if text == nil || pcln == nil || pcln.Size > traceTLSGoMaxTable {
		return "", fmt.Errorf("probe needs bounded Mach-O Go text and pclntab")
	}
	data, err := pcln.Data()
	if err != nil {
		return "", err
	}
	table, err := gosym.NewTable(nil, gosym.NewLineTable(data, text.Addr))
	if err != nil {
		return "", err
	}
	machine := elf.EM_AARCH64
	object, buffer, length, goroutine, stack := "R_X0", "R_X1", "R_X2", "R_X28", "R_SP"
	if file.Cpu == macho.CpuAmd64 {
		machine = elf.EM_X86_64
		object, buffer, length, goroutine, stack = "R_RAX", "R_RBX", "R_RCX", "R_R14", "R_RSP"
	} else if file.Cpu != macho.CpuArm64 {
		return "", fmt.Errorf("unsupported Go probe Mach-O CPU")
	}
	module, err := darwinTLSModuleName(path)
	if err != nil {
		return "", err
	}
	var script strings.Builder
	script.WriteString(`#pragma D option quiet
#pragma D option strsize=256
#pragma D option bufsize=2m
#pragma D option dynvarsize=2m
#pragma D option temporal
uint64_t edc_buffer[pid_t,uint64_t,uint64_t,uint64_t];
uint64_t edc_requested[pid_t,uint64_t,uint64_t,uint64_t];
uint64_t edc_stack_offset[pid_t,uint64_t,uint64_t,uint64_t];
uint64_t edc_thread[pid_t,uint64_t,uint64_t,uint64_t];
BEGIN { printf("EDC_PROBES_READY\n"); }
`)
	for direction, name := range []string{traceTLSGoRead, traceTLSGoWrite} {
		function := table.LookupFunc(name)
		if function == nil || function.Entry < text.Addr || function.End <= function.Entry || function.End-text.Addr > text.Size || function.End-function.Entry > traceTLSGoMaxCode {
			return "", fmt.Errorf("invalid Mach-O Go function %s", name)
		}
		code := make([]byte, function.End-function.Entry)
		if _, err := text.ReadAt(code, int64(function.Entry-text.Addr)); err != nil {
			return "", err
		}
		if _, err := traceTLSGoEntry(machine, code, 0); err != nil {
			return "", err
		}
		if _, err := traceTLSGoReturns(machine, code, 0); err != nil {
			return "", err
		}
		// Go stack growth retries preserve stack.hi-SP when they re-enter the same call.
		prefix := fmt.Sprintf("pid$target:%s:%s:", module, strings.NewReplacer("/", "?", "(", "?", ")", "?").Replace(name))
		fmt.Fprintf(&script, `%sentry
{
 this->g=uregs[%s];
 this->buffer=uregs[%s];
 this->lo=*(userland uint64_t *)this->g;
 this->hi=*(userland uint64_t *)(this->g+8);
 this->frame=this->hi-uregs[%s];
 this->thread=edc_thread[pid,this->g,%d,this->frame];
 @edc_pending = sum((int64_t)(this->thread == 0));
 edc_buffer[pid,this->g,%d,this->frame]=this->buffer;
 edc_requested[pid,this->g,%d,this->frame]=uregs[%s];
 edc_stack_offset[pid,this->g,%d,this->frame]=(this->buffer >= this->lo && this->buffer < this->hi) ? this->hi-this->buffer : 0;
 edc_thread[pid,this->g,%d,this->frame]=this->thread ? this->thread : tid;
}
`, prefix, goroutine, buffer, stack, direction, direction, direction, length, direction, direction)
		fmt.Fprintf(&script, `%sreturn
{
 this->g=uregs[%s];
 this->hi=*(userland uint64_t *)(this->g+8);
 this->frame=this->hi-uregs[%s];
 this->size=uregs[%s];
 this->requested=edc_requested[pid,this->g,%d,this->frame];
 this->offset=edc_stack_offset[pid,this->g,%d,this->frame];
 this->buffer=this->offset ? this->hi-this->offset : edc_buffer[pid,this->g,%d,this->frame];
}
%sreturn
/edc_thread[pid,this->g,%d,this->frame]/
{
 printf("EDC_GO_PAIR %d %%llu %%llu %%llu %%lld %%llu %%llu\n",this->g,edc_thread[pid,this->g,%d,this->frame],(uint64_t)tid,(int64_t)this->size,this->requested,this->frame);
 @edc_pending = sum((int64_t)-1);
}
%sreturn
/edc_thread[pid,this->g,%d,this->frame] && (int64_t)this->size > 0 && this->size <= this->requested/
{
 printf("EDC_GO_PLAINTEXT %d %%s\n",copyinstr(this->buffer,this->size < 128 ? this->size : 128));
}
%sreturn
{
 edc_buffer[pid,this->g,%d,this->frame]=0;
 edc_requested[pid,this->g,%d,this->frame]=0;
 edc_stack_offset[pid,this->g,%d,this->frame]=0;
 edc_thread[pid,this->g,%d,this->frame]=0;
}
`, prefix, goroutine, stack, object, direction, direction, direction, prefix, direction, direction, direction, prefix, direction, direction, prefix, direction, direction, direction, direction)

	}
	script.WriteString("ERROR { printf(\"EDC_PROBE_FAULT\\n\"); }\nEND { printa(\"EDC_GO_PENDING %@d\\n\",@edc_pending); }\n")
	return script.String(), nil
}

func TestDarwinTLSExpansionFixtures(t *testing.T) {
	t.Run("SecureTransport", func(t *testing.T) {
		binary := buildDarwinTLSExpansionFixture(t, false, runtime.GOARCH)
		server, received := darwinTLSProbeServer(t, 0)
		command := exec.Command(binary, strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port))
		command.Stdin = strings.NewReader("\n")
		if output, err := command.CombinedOutput(); err != nil || !received.Load() {
			t.Fatalf("SecureTransport HTTPS: received=%t error=%v\n%s", received.Load(), err, output)
		}
	})
	t.Run("Go", func(t *testing.T) {
		binary := buildDarwinTLSExpansionFixture(t, true, runtime.GOARCH)
		command := exec.Command(binary)
		command.Stdin = strings.NewReader("\n")
		if output, err := command.CombinedOutput(); err != nil || !bytes.Contains(output, []byte("completed 64 HTTPS requests")) {
			t.Fatalf("Go HTTPS: %v\n%s", err, output)
		}
	})
}

func TestDarwinTLSExpansionProbeSyntax(t *testing.T) {
	binary := buildDarwinTLSExpansionFixture(t, true, runtime.GOARCH)
	goScript, err := darwinGoTLSProbeScript(binary)
	if err != nil {
		t.Fatal(err)
	}
	secureScript, err := darwinTLSScript([]traceTLSTarget{{path: "/System/Library/Frameworks/Security.framework/Security", symbols: []string{"SSLRead", "SSLWrite", "SSLClose"}}})
	if err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{"Go": goScript, "SecureTransport": darwinSecureTransportProbe, "SecureTransportBinary": secureScript} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "probe.d")
			if err := os.WriteFile(path, []byte(strings.ReplaceAll(script, "pid$target:", "profile:")), 0600); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command("/usr/sbin/dtrace", "-e", "-Z", "-s", path).CombinedOutput(); err != nil {
				t.Fatalf("probe syntax: %v\n%s", err, output)
			}
		})
	}
}

func runDarwinTLSExpansionLive(t *testing.T, binary, script, readyPrefix string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := exec.CommandContext(ctx, binary, args...)
	client.Env = append(os.Environ(), "SSLKEYLOGFILE=")
	input, err := client.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	stdout, err := client.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var clientErrors bytes.Buffer
	client.Stderr = &clientErrors
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Process.Kill()
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, readyPrefix) {
		_ = client.Process.Kill()
		_ = client.Wait()
		t.Fatalf("fixture readiness: %v %q", err, line)
	}
	t.Logf("fixture ready: %s", strings.TrimSpace(line))
	clientDone := make(chan error, 1)
	clientCollected := false
	go func() {
		_, readErr := io.Copy(io.Discard, reader)
		waitErr := client.Wait()
		if readErr != nil {
			waitErr = readErr
		}
		clientDone <- waitErr
	}()
	defer func() {
		if !clientCollected {
			_ = client.Process.Kill()
			<-clientDone
		}
	}()
	trace := exec.CommandContext(ctx, "/usr/sbin/dtrace", "-q", "-p", strconv.Itoa(client.Process.Pid), "-s", "/dev/stdin")
	trace.Stdin = strings.NewReader(script)
	traceOutput, err := trace.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var diagnostic bytes.Buffer
	trace.Stderr = &diagnostic
	if err := trace.Start(); err != nil {
		t.Fatal(err)
	}
	traceDone, ready := make(chan error, 1), make(chan struct{})
	var captured bytes.Buffer
	go func() {
		scanner := bufio.NewScanner(traceOutput)
		announced := false
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Fprintln(&captured, line)
			if (line == "EDC_PROBES_READY" || line == "EDC_READY") && !announced {
				close(ready)
				announced = true
			}
		}
		readErr := scanner.Err()
		waitErr := trace.Wait()
		if readErr != nil {
			waitErr = readErr
		}
		traceDone <- waitErr
	}()
	traceCollected := false
	defer func() {
		if !traceCollected {
			_ = trace.Process.Kill()
			<-traceDone
		}
	}()
	select {
	case <-ready:
	case err := <-traceDone:
		traceCollected = true
		t.Fatalf("attach TLS expansion probe: %v\n%s\n%s", err, diagnostic.String(), captured.String())
	case <-ctx.Done():
		t.Fatal("probe readiness timed out")
	}
	if _, err := input.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-clientDone:
		clientCollected = true
		if err != nil {
			t.Fatalf("TLS fixture: %v\n%s", err, clientErrors.String())
		}
	case <-ctx.Done():
		t.Fatal("TLS fixture timed out")
	}
	select {
	case err := <-traceDone:
		traceCollected = true
		if err != nil {
			t.Fatalf("TLS probe: %v\n%s\n%s", err, diagnostic.String(), captured.String())
		}
	case <-time.After(500 * time.Millisecond):
		_ = trace.Process.Signal(os.Interrupt)
		select {
		case err := <-traceDone:
			traceCollected = true
			if err != nil {
				t.Fatalf("stop TLS probe: %v\n%s", err, diagnostic.String())
			}
		case <-ctx.Done():
			t.Fatal("TLS probe did not stop")
		}
	case <-ctx.Done():
		t.Fatal("TLS probe did not exit with fixture")
	}
	if strings.Contains(captured.String(), "EDC_PROBE_FAULT") || strings.Contains(strings.ToLower(diagnostic.String()), "drop") {
		t.Fatalf("probe fault or drop\n%s\n%s", diagnostic.String(), captured.String())
	}
	return captured.String()
}

func TestDarwinSecureTransportPlaintextLive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("SecureTransport DTrace probe requires root")
	}
	binary := buildDarwinTLSExpansionFixture(t, false, runtime.GOARCH)
	server, received := darwinTLSProbeServer(t, 0)
	output := runDarwinTLSExpansionLive(t, binary, darwinSecureTransportProbe, "EDC_CLIENT_READY", strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port))
	if !received.Load() || !strings.Contains(output, "GET /keyless-probe HTTP/1.1") || !strings.Contains(output, "HTTP/1.1 200 OK") {
		t.Fatalf("SecureTransport plaintext missing\n%s", output)
	}
	t.Log("SecureTransport request and response plaintext observed without a key log")
}

func TestDarwinGoTLSProbeMachOArchitectures(t *testing.T) {
	for _, arch := range []string{"arm64", "amd64"} {
		t.Run(arch, func(t *testing.T) {
			binary := buildDarwinTLSExpansionFixture(t, true, arch)
			script, err := darwinGoTLSProbeScript(binary)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(script, "\n") {
				line = strings.TrimSuffix(strings.TrimSpace(line), ",")
				if !strings.HasPrefix(line, "pid$target:") {
					continue
				}
				fields := strings.Split(line, ":")
				if len(fields) != 4 || fields[3] != "entry" && fields[3] != "return" {
					t.Fatalf("Go TLS requires named entry/return probes on macOS arm64: %s", line)
				}
			}
			register := "R_X28"
			if arch == "amd64" {
				register = "R_R14"
			}
			if !strings.Contains(script, "uregs["+register+"]") || strings.Contains(script, "%!") {
				t.Fatal("invalid architecture-specific Go probe")
			}
		})
	}
}

func TestDarwinGoTLSPlaintextLive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("Go TLS DTrace probe requires root")
	}
	binary := buildDarwinTLSExpansionFixture(t, true, runtime.GOARCH)
	script, err := darwinGoTLSProbeScript(binary)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"stress", "stack", "errors"} {
		t.Run(mode, func(t *testing.T) {
			output := runDarwinTLSExpansionLive(t, binary, script, "ready ", mode)
			if !strings.Contains(output, "EDC_GO_PENDING 0\n") {
				t.Fatalf("unmatched Go returns\n%s", output)
			}
			pairs, migrations := 0, 0
			for _, line := range strings.Split(output, "\n") {
				if !strings.HasPrefix(line, "EDC_GO_PAIR ") {
					continue
				}
				var direction int
				var goroutine, entryThread, returnThread, requested, frame uint64
				var count int64
				if _, err := fmt.Sscanf(line, "EDC_GO_PAIR %d %d %d %d %d %d %d", &direction, &goroutine, &entryThread, &returnThread, &count, &requested, &frame); err != nil || direction < 0 || direction > 1 || goroutine == 0 || frame == 0 || entryThread == 0 || returnThread == 0 || count < 0 || uint64(count) > requested {
					t.Fatalf("invalid Go return: %q error=%v", line, err)
				}
				pairs++
				if entryThread != returnThread {
					migrations++
				}
			}
			if pairs == 0 {
				t.Fatalf("Go entry/return pairs missing\n%s", output)
			}
			switch mode {
			case "stress":
				if strings.Count(output, "GET /go-tls-probe-") < 64 || strings.Count(output, "HTTP/1.1 200 OK") < 64 {
					t.Fatalf("incomplete Go stress capture\n%s", output)
				}
			case "stack":
				if strings.Count(output, "GET /stack-") < 16 || strings.Count(output, "HTTP/1.1 200 OK") < 16 {
					t.Fatalf("Go stack-growth plaintext missing\n%s", output)
				}
			case "errors":
				if strings.Contains(output, "EDC_GO_PLAINTEXT") {
					t.Fatalf("empty or failed Go calls emitted plaintext\n%s", output)
				}
			}
			t.Logf("Go TLS entry/return pairs=%d, thread migrations=%d, no key log", pairs, migrations)
		})
	}
}

func TestDarwinSecureTransportExports(t *testing.T) {
	library, err := purego.Dlopen("/System/Library/Frameworks/Security.framework/Security", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		t.Fatal(err)
	}
	defer purego.Dlclose(library)
	for _, name := range []string{"SSLRead", "SSLWrite", "SSLClose"} {
		address, err := purego.Dlsym(library, name)
		if err != nil || address == 0 {
			t.Fatalf("SecureTransport export %s: address=%x error=%v", name, address, err)
		}
	}
}

func TestDarwinSecureTransportBinaryLive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("SecureTransport binary DTrace capture requires root")
	}
	binary := buildDarwinTLSExpansionFixture(t, false, runtime.GOARCH)
	server, received := darwinTLSProbeServer(t, 30*time.Millisecond)
	script, err := darwinTLSScript([]traceTLSTarget{{path: "/System/Library/Frameworks/Security.framework/Security", symbols: []string{"SSLRead", "SSLWrite", "SSLClose"}}})
	if err != nil {
		t.Fatal(err)
	}
	output := runDarwinTLSExpansionLive(t, binary, script, "EDC_CLIENT_READY", strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port))
	tracker := newPKTAPHTTPTracker(traceScope{payload: true, payloadAll: true}, 0)
	var decoder darwinTLSDecoder
	request, response, closed := false, false, false
	var events []captureEvent
	for _, line := range strings.Split(output, "\n") {
		record, err := decoder.line(line)
		if err != nil {
			t.Fatalf("decode SecureTransport: %v\n%s", err, output)
		}
		if record == nil || record.fdMeta != 0 || record.goPhase != 0 {
			continue
		}
		closed = closed || record.closed
		found, unmapped := tracker.addDarwinTLS(*record, 0, time.Now())
		if unmapped {
			t.Fatal("unexpected port filter on unfiltered capture")
		}
		events = append(events, found...)
	}
	events = append(events, tracker.sweep(time.Now(), true)...)
	for _, event := range events {
		if strings.Contains(event.Payload, "Bearer fixture-secret") {
			t.Fatal("SecureTransport authorization header was not masked")
		}
		if event.TLS && event.Path == "/keyless-probe" {
			request = request || event.Method == "GET" && event.Status == 0
			if event.Status == 200 {
				response = true
				if !strings.Contains(event.Payload, traceEscapeText([]byte{'o', 0, 'k', 255})) {
					t.Fatal("SecureTransport binary response was not preserved")
				}
				if event.LatencyMS == nil || *event.LatencyMS < 20 {
					t.Fatalf("response latency=%v", event.LatencyMS)
				}
			}
		}
	}
	if !received.Load() || !request || !response || !closed || tracker.lost != 0 || tracker.buffered != 0 || decoder.record != nil {
		t.Fatalf("HTTPS=%t request=%t response=%t close=%t lost=%d buffered=%d\n%s", received.Load(), request, response, closed, tracker.lost, tracker.buffered, output)
	}
	t.Log("SecureTransport binary HTTPS capture, masking, latency and close cleanup passed without a key log")
}

func TestDarwinSecureTransportPartialIOLive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("SecureTransport partial I/O DTrace validation requires root")
	}
	const source = `
#include <stddef.h>
#include <stdio.h>
#include <string.h>
#include <stdint.h>
static int writes;
static int reads;
static const char request[]="GET /partial HTTP/1.1\r\nHost: localhost\r\n\r\n";
static const char response[]="HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\no\0k\377";
int SSLWrite(void *context,const void *data,size_t length,size_t *processed) {
 if (processed==(size_t *)(uintptr_t)1) return -9807;
 if (length==1) { *processed=2; return 0; }
 *processed=writes++==0 ? 3 : length;
 return writes==1 ? -9803 : 0;
}
int SSLRead(void *context,void *data,size_t length,size_t *processed) {
 if (processed==(size_t *)(uintptr_t)1) return -9807;
 *processed=reads++==0 ? 5 : sizeof(response)-1-5;
 memcpy(data,response+(reads==1 ? 0 : 5),*processed);
 return reads==1 ? -9803 : -9805;
}
int SSLClose(void *context) { return 0; }
int main(void) {
 int context=1;
 puts("EDC_CLIENT_READY"); fflush(stdout);
 if(getchar()==EOF) return 2;
 size_t n=0;
 SSLWrite(&context,request,sizeof(request)-1,&n);
 SSLWrite(&context,request+n,sizeof(request)-1-n,&n);
 SSLWrite(&context,request,sizeof(request)-1,(size_t *)(uintptr_t)1);
 SSLWrite(&context,request,0,&n);
 SSLWrite(&context,request,1,&n);
 char buffer[4096];
 SSLRead(&context,buffer,sizeof(buffer),&n);
 SSLRead(&context,buffer,sizeof(buffer),&n);
 SSLRead(&context,buffer,sizeof(buffer),(size_t *)(uintptr_t)1);
 SSLClose(&context);
 return 0;
}`
	directory := t.TempDir()
	path, binary := filepath.Join(directory, "client.c"), filepath.Join(directory, "client")
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "/usr/bin/clang", "-O0", path, "-o", binary).CombinedOutput(); err != nil {
		t.Fatalf("compile partial I/O fixture: %v\n%s", err, output)
	}
	script, err := darwinTLSScript([]traceTLSTarget{{path: binary, symbols: []string{"SSLRead", "SSLWrite", "SSLClose"}}})
	if err != nil {
		t.Fatal(err)
	}
	output := runDarwinTLSExpansionLive(t, binary, script, "EDC_CLIENT_READY")
	var decoder darwinTLSDecoder
	var payloads [2][]byte
	closed := false
	for _, line := range strings.Split(output, "\n") {
		record, err := decoder.line(line)
		if err != nil {
			t.Fatalf("partial I/O decode: %v\n%s", err, output)
		}
		if record == nil || record.fdMeta != 0 || record.goPhase != 0 {
			continue
		}
		if record.closed {
			closed = true
			continue
		}
		direction := 0
		if record.sent {
			direction = 1
		}
		if int(record.offset) != len(payloads[direction]) {
			t.Fatalf("partial I/O offset=%d expected=%d", record.offset, len(payloads[direction]))
		}
		payloads[direction] = append(payloads[direction], record.payload...)
	}
	request := "GET /partial HTTP/1.1\r\nHost: localhost\r\n\r\n"
	response := "HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\no\x00k\xff"
	if string(payloads[1]) != request || string(payloads[0]) != response || !closed || decoder.record != nil {
		t.Fatalf("partial I/O request=%q response=%q close=%t\n%s", payloads[1], payloads[0], closed, output)
	}
	t.Log("partial success, EOF bytes, zero-length, failed and oversized I/O preserve exact TLS stream offsets")
}

func TestDarwinTLSExpansionDiscovery(t *testing.T) {
	for _, goTLS := range []bool{false, true} {
		t.Run(fmt.Sprint(goTLS), func(t *testing.T) {
			binary := buildDarwinTLSExpansionFixture(t, goTLS, runtime.GOARCH)
			targets, err := inspectDarwinTLSFile(binary)
			if err != nil || len(targets) != 1 {
				t.Fatalf("targets=%#v error=%v", targets, err)
			}
			if goTLS && targets[0].goMachine == 0 || !goTLS && targets[0].darwinModule != "Security" {
				t.Fatalf("wrong TLS backend: %#v", targets[0])
			}
			script, err := darwinTLSScript(targets)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(script, "copyinstr") || strings.Contains(script, "copyin(") {
				t.Fatal("binary collector uses string or kernel memory reads")
			}
			path := filepath.Join(t.TempDir(), "capture.d")
			if err := os.WriteFile(path, []byte(strings.ReplaceAll(script, "pid$target:", "profile:")), 0600); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command("/usr/sbin/dtrace", "-e", "-Z", "-s", path).CombinedOutput(); err != nil {
				t.Fatalf("collector script syntax: %v\n%s", err, output)
			}
			previous := readDarwinTLSMappings
			t.Cleanup(func() { readDarwinTLSMappings = previous })
			readDarwinTLSMappings = func(context.Context) (map[uint32][]string, error) { return map[uint32][]string{42: {binary}}, nil }
			selected, err := findDarwinTLSTargets(context.Background(), traceTLSAuto, "")
			if err != nil || len(selected) != 1 || selected[0].pid != 42 {
				t.Fatalf("discovery=%#v error=%v", selected, err)
			}
			selected, err = findDarwinTLSTargets(context.Background(), traceTLSMode(binary), "")
			if err != nil || len(selected) != 1 {
				t.Fatalf("explicit discovery=%#v error=%v", selected, err)
			}
		})
	}
}
func TestDarwinGoTLSPollFDLayout(t *testing.T) {
	for _, arch := range []string{"arm64", "amd64"} {
		t.Run(arch, func(t *testing.T) {
			path := buildDarwinTLSExpansionFixture(t, true, arch)
			file, err := macho.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			data, err := file.DWARF()
			if err != nil {
				t.Fatal(err)
			}
			reader := data.Reader()
			for {
				entry, err := reader.Next()
				if err != nil {
					t.Fatal(err)
				}
				if entry == nil {
					break
				}
				if entry.Tag != dwarf.TagStructType || entry.Val(dwarf.AttrName) != "internal/poll.FD" {
					continue
				}
				value, err := data.Type(entry.Offset)
				if err != nil {
					t.Fatal(err)
				}
				fields := value.(*dwarf.StructType).Field
				offsets := map[string]int64{}
				for _, field := range fields {
					offsets[field.Name] = field.ByteOffset
				}
				if offsets["Sysfd"] != 16 || offsets["isFile"] != 50 {
					t.Fatalf("unsupported poll FD offsets=%v", offsets)
				}
				return
			}
			t.Fatal("poll FD DWARF layout not found")
		})
	}
}

func TestDarwinHTTPSExpansionCaptureLive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("expanded macOS HTTPS collector requires root")
	}
	for _, goTLS := range []bool{false, true} {
		name := "SecureTransport"
		if goTLS {
			name = "Go"
		}
		t.Run(name, func(t *testing.T) {
			binary := buildDarwinTLSExpansionFixture(t, goTLS, runtime.GOARCH)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			port := 0
			var args []string
			if !goTLS {
				server, _ := darwinTLSProbeServer(t, 30*time.Millisecond)
				port = server.Listener.Addr().(*net.TCPAddr).Port
				args = []string{strconv.Itoa(port)}
			}
			client := exec.CommandContext(ctx, binary, args...)
			client.Env = append(os.Environ(), "SSLKEYLOGFILE=")
			input, err := client.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			output, err := client.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var diagnostic bytes.Buffer
			client.Stderr = &diagnostic
			if err := client.Start(); err != nil {
				t.Fatal(err)
			}
			var done chan error
			collected := false
			defer func() {
				if !collected {
					_ = client.Process.Kill()
					if done == nil {
						_ = client.Wait()
					} else {
						<-done
					}
				}
			}()
			reader := bufio.NewReader(output)
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if goTLS {
				fields := strings.Fields(line)
				if len(fields) != 3 {
					_ = client.Process.Kill()
					_ = client.Wait()
					t.Fatalf("Go ready=%q", line)
				}
				address := strings.TrimPrefix(fields[2], "https://")
				_, value, err := net.SplitHostPort(address)
				if err != nil {
					t.Fatal(err)
				}
				port, err = strconv.Atoi(value)
				if err != nil {
					t.Fatal(err)
				}
			}
			done = make(chan error, 1)
			go func() {
				_, readErr := io.Copy(io.Discard, reader)
				waitErr := client.Wait()
				if readErr != nil {
					waitErr = readErr
				}
				done <- waitErr
			}()

			targets, err := findDarwinTLSTargets(ctx, traceTLSAuto, "client")
			if err != nil {
				t.Fatal(err)
			}
			var selected []traceTLSTarget
			for _, target := range targets {
				if target.pid == uint32(client.Process.Pid) {
					selected = append(selected, target)
				}
			}
			if len(selected) != 1 {
				t.Fatalf("TLS discovery=%#v", selected)
			}
			previous := captureAttachedHook
			t.Cleanup(func() { captureAttachedHook = previous })
			captureAttachedHook = func() {
				if _, err := input.Write([]byte("go\n")); err != nil {
					t.Error(err)
				}
			}
			api, err := loadDarwinHTTPPCAP()
			if err != nil {
				t.Fatal(err)
			}
			originalStats := api.stats
			var packetStats darwinPCAPStats
			api.stats = func(handle uintptr, stats *darwinPCAPStats) int32 {
				result := originalStats(handle, stats)
				packetStats = *stats
				return result
			}
			t.Cleanup(func() { api.stats = originalStats })
			previousTrackerHook := captureTrackerHook
			t.Cleanup(func() { captureTrackerHook = previousTrackerHook })
			previousPacketHook := capturePacketHook
			t.Cleanup(func() { capturePacketHook = previousPacketHook })
			previousTLSHook := captureTLSRecordHook
			t.Cleanup(func() { captureTLSRecordHook = previousTLSHook })
			var replay darwinPKTAPReplay
			if goTLS {
				captureTLSRecordHook = func(record darwinTLSRecord, port uint16, now time.Time) {
					if record.pid == uint32(client.Process.Pid) {
						replay.addTLS(record, port, now)
					}
				}
			}
			if goTLS {
				capturePacketHook = func(packet pktapTCPPacket, now time.Time) {
					if packet.pid == 0 || packet.pid == uint32(client.Process.Pid) {
						replay.add(packet, now)
					}
				}
			}
			lossCounts := map[string]int{}
			var lossSamples []string
			captureTrackerHook = func(tracker *pktapHTTPTracker) {
				tracker.lossHook = func(reason string, packet httpPacket, detail string) {
					lossCounts[reason]++
					if len(lossSamples) < 8 {
						lossSamples = append(lossSamples, fmt.Sprintf("%s pid=%d sent=%t decrypted=%t local=%s remote=%s %s", reason, packet.pid, packet.sent, packet.decrypted, packet.source, packet.destination, detail))
					}
				}
			}
			requests, responses := 0, 0
			summary, err := collectDarwinHTTPEvents(traceScope{protocol: "http", process: "client", tls: traceTLSMode(binary), tlsFinder: &traceTLSFinder{targets: selected}, side: "client", port: uint16(port), payload: true, payloadAll: true}, 2*time.Second, func(event captureEvent) error {
				if !event.TLS || event.PID != uint32(client.Process.Pid) {
					return nil
				}
				if goTLS {
					if value, ok := darwinReplayHTTPEvent(event); ok {
						replay.Expected = append(replay.Expected, value)
					}
				}
				if strings.Contains(event.Payload, "Bearer fixture-secret") {
					return fmt.Errorf("authorization header was not masked")
				}
				if event.Method == "GET" && event.Status == 0 {
					requests++
				}
				if event.Status == 200 {
					responses++
					if event.Method != "GET" || event.Path == "" || event.LatencyMS == nil {
						return fmt.Errorf("TLS response lacks matched request: %#v", event)
					}
					if !goTLS && (!strings.Contains(event.Payload, traceEscapeText([]byte{'o', 0, 'k', 255})) || *event.LatencyMS < 20) {
						return fmt.Errorf("SecureTransport body or latency was not preserved")
					}
				}
				return nil
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				collected = true
				if err != nil {
					t.Fatalf("fixture=%v\n%s", err, diagnostic.String())
				}
			case <-ctx.Done():
				t.Fatal("fixture timed out")
			}
			want := 1
			if goTLS {
				want = 64
			}
			if requests != want || responses != want || summary.TLSUnmapped != 0 || summary.LostEvents != 0 {
				if goTLS && summary.LostEvents != 0 {
					replay.save(t)
				}
				t.Fatalf("requests=%d responses=%d expected=%d pcap=%#v decoder_loss=%d loss_counts=%v loss_samples=%v summary=%#v", requests, responses, want, packetStats, summary.LostEvents-uint64(packetStats.dropped)-uint64(packetStats.interfaceDropped), lossCounts, lossSamples, summary)
			}
			t.Log("automatic TLS discovery, port/side filters, response association and payload capture passed")
		})
	}
}

func TestDarwinTLSGoRejectsStrippedAndIgnoresNoTLS(t *testing.T) {
	binary := buildDarwinTLSExpansionFixture(t, true, runtime.GOARCH)
	if output, err := exec.Command("/usr/bin/strip", "-x", binary).CombinedOutput(); err != nil {
		t.Fatalf("strip Go fixture: %v\n%s", err, output)
	}
	if _, err := inspectDarwinTLSFile(binary); err == nil || !strings.Contains(err.Error(), "stripped") {
		t.Fatalf("stripped Go target error=%v", err)
	}
	directory := t.TempDir()
	source, plain := filepath.Join(directory, "plain.go"), filepath.Join(directory, "plain")
	if err := os.WriteFile(source, []byte("package main\nimport \"fmt\"\nfunc main(){fmt.Println(1)}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "go", "build", "-o", plain, source).CombinedOutput(); err != nil {
		t.Fatalf("compile non-TLS Go: %v\n%s", err, output)
	}
	targets, err := inspectDarwinTLSFile(plain)
	if err != nil || len(targets) != 0 {
		t.Fatalf("non-TLS Go is a TLS target: %#v %v", targets, err)
	}
}

func darwinGoTLSFDDiagnosticScript(module string) string {
	receiver, g := "R_X0", "R_X28"
	if runtime.GOARCH == "amd64" {
		receiver, g = "R_RAX", "R_R14"
	}
	template := `
pid$target:@M@:internal?poll.?*FD?.Read:entry,
pid$target:@M@:internal?poll.?*FD?.Write:entry
{
 printf("EDC_DIAG_FD %llu %llu %llu %llu %lld %u %llu %llu\n",uregs[@G@],edc_go_current[pid,uregs[@G@],0],edc_go_current[pid,uregs[@G@],1],uregs[@R@],*(userland int64_t *)(uregs[@R@]+16),(uint32_t)*(userland uint8_t *)(uregs[@R@]+50),this->edc_g,this->edc_poll);
}
`
	return strings.NewReplacer("@M@", module, "@G@", g, "@R@", receiver).Replace(template)
}

func TestDarwinGoTLSFDDiagnosticSyntax(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostic.d")
	binary := buildDarwinTLSExpansionFixture(t, true, runtime.GOARCH)
	targets, err := inspectDarwinTLSFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	script, err := darwinTLSScript(targets)
	if err != nil {
		t.Fatal(err)
	}
	module, err := darwinTLSModuleName(binary)
	if err != nil {
		t.Fatal(err)
	}
	script += darwinGoTLSFDDiagnosticScript(module)
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(script, "pid$target:", "profile:")), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("/usr/sbin/dtrace", "-e", "-Z", "-s", path).CombinedOutput(); err != nil {
		t.Fatalf("FD diagnostic syntax: %v\n%s", err, output)
	}
}

func TestDarwinGoTLSFDMappingDiagnosticLive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("Go TLS FD diagnostic requires root")
	}
	binary := buildDarwinTLSExpansionFixture(t, true, runtime.GOARCH)
	targets, err := inspectDarwinTLSFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	script, err := darwinTLSScript(targets)
	if err != nil {
		t.Fatal(err)
	}
	module, err := darwinTLSModuleName(binary)
	if err != nil {
		t.Fatal(err)
	}
	output := runDarwinTLSExpansionLive(t, binary, script+darwinGoTLSFDDiagnosticScript(module), "ready ")
	var decoder darwinTLSDecoder
	var cache *darwinTLSAddressCache
	order := newDarwinTLSGoOrder()
	counts := map[string]int{}
	samples := map[string]int{}
	for _, line := range strings.Split(output, "\n") {
		category := ""
		if strings.HasPrefix(line, "EDC_DIAG_FD ") {
			counts["poll_calls"]++
			var g, read, write, poll, localG, localPoll uint64
			var fd int64
			var isFile uint32
			if _, err := fmt.Sscanf(line, "EDC_DIAG_FD %d %d %d %d %d %d %d %d", &g, &read, &write, &poll, &fd, &isFile, &localG, &localPoll); err != nil {
				t.Fatal(err)
			}
			category := "inactive_poll"
			if read != 0 || write != 0 {
				category = "active_poll"
				counts["active_poll"]++
				if isFile == 0 && fd >= 0 {
					counts["active_network_poll"]++
				}
			}
			if g != localG || poll != localPoll {
				counts["clause_state_mismatch"]++
			}
			limit := 8
			if category == "inactive_poll" {
				limit = 2
			}
			if samples[category] < limit {
				t.Logf("poll g=%x read=%x write=%x pfd=%x fd=%d file=%d clause_g=%x clause_pfd=%x", g, read, write, poll, fd, isFile, localG, localPoll)
				samples[category]++
			}
			continue
		}
		r, err := decoder.line(line)
		if err != nil {
			t.Fatalf("decode: %v\n%s", err, line)
		}
		if r == nil {
			continue
		}
		if cache == nil {
			cache = newDarwinTLSAddressCache(r.pid)
			cache.lookup = func(uint32, int32) (string, string) { return "", "" }
		}
		switch r.fdMeta {
		case 1:
			category = "bind"
		case 2:
			category = "address"
		case 3:
			category = "reset"
		}
		if category != "" {
			counts[category]++
			if err := cache.metadata(*r); err != nil {
				t.Fatal(err)
			}
			if samples[category] < 8 {
				t.Logf("%s fd=%d object=%x kind=%d address=%s", category, r.fd, r.object, r.fdKind, r.address)
				samples[category]++
			}
			continue
		}
		if err := cache.decorate(r); err != nil {
			t.Fatal(err)
		}
		ready, err := order.add(*r)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range ready {
			if record.closed {
				cache.forget(record.object)
			}
		}
		if r.goPhase != 0 || r.closed {
			continue
		}

		counts["data"]++
		if r.fd < 0 {
			counts["data_without_fd"]++
		}
		if r.source == "" && r.destination == "" {
			counts["data_without_address"]++
			if samples["unmapped"] < 8 {
				t.Logf("unmapped object=%x fd=%d sent=%t offset=%d captured=%d full=%d", r.object, r.fd, r.sent, r.offset, len(r.payload), r.size)
				samples["unmapped"]++
			}
		}
	}
	t.Logf("FD diagnostic counts=%v", counts)
	if counts["data"] == 0 {
		t.Fatal("no Go TLS binary records captured")
	}
	if counts["data_without_address"] > 0 {
		t.Fatalf("FD diagnostic complete: %d records still lack socket addresses", counts["data_without_address"])
	}
}

func TestDarwinTLSDiscoveryKeepsSecureTransportWhenGoABIIsUnsupported(t *testing.T) {
	directory := t.TempDir()
	source, binary := filepath.Join(directory, "mixed.go"), filepath.Join(directory, "mixed")
	preamble := `package main
/*
#cgo CFLAGS: -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Security
#include <Security/SecureTransport.h>
*/
import "C"
`
	code := strings.Replace(darwinGoTLSFixture, "package main", preamble, 1)
	code = strings.Replace(code, "func main() {", "func main() {\nvar processed C.size_t\nC.SSLRead(C.SSLContextRef(0),nil,0,&processed)\nC.SSLWrite(C.SSLContextRef(0),nil,0,&processed)\n", 1)
	if err := os.WriteFile(source, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-o", binary, source)
	command.Env = append(os.Environ(), "CGO_ENABLED=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile mixed TLS metadata fixture: %v\n%s", err, output)
	}
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.Index(data, []byte(traceTLSGoBuildMagic))
	if at < 0 || len(info.GoVersion) != 8 || data[at+32] != 8 {
		t.Fatal("unexpected inline Go build version layout")
	}
	copy(data[at+33:at+41], []byte("go1.24.0"))
	if err := os.WriteFile(binary, data, 0755); err != nil {
		t.Fatal(err)
	}
	targets, err := inspectDarwinTLSFile(binary)
	if err != nil || len(targets) != 1 || targets[0].darwinModule != "Security" || !strings.Contains(targets[0].darwinNotice, "go1.24.0") {
		t.Fatalf("independent SecureTransport lost: targets=%#v error=%v", targets, err)
	}
}
