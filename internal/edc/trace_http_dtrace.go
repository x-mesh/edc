package edc

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

var darwinTLSFunctions = []string{"SSL_read", "SSL_write", "SSL_read_ex", "SSL_write_ex", "SSL_free", "SSL_set_fd"}

type darwinTLSRecord struct {
	goPhase              uint8
	fdMeta               uint8
	fdKind               uint8
	address              string
	source, destination  string
	goroutine, frame     uint64
	pid                  uint32
	wallNS, object, size uint64
	sent, closed         bool
	fd                   int32
	offset               uint32
	process              string
	payload              []byte
}
type darwinTLSDecoder struct {
	record   *darwinTLSRecord
	captured int
}

func (d *darwinTLSDecoder) line(line string) (*darwinTLSRecord, error) {
	line = strings.TrimSpace(line)
	writeSnapshot := strings.HasPrefix(line, "EDC_GO_WRITE ")
	if writeSnapshot {
		line = "EDC_TLS " + strings.TrimPrefix(line, "EDC_GO_WRITE ")
	}
	if strings.HasPrefix(line, "EDC_FD_") {
		if d.record != nil {
			return nil, fmt.Errorf("FD metadata interrupted a TLS record")
		}
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "EDC_FD_RESET" {
			pid, e1 := strconv.ParseUint(fields[1], 10, 31)
			fd, e2 := strconv.ParseUint(fields[2], 10, 31)
			if e1 != nil || e2 != nil || pid == 0 {
				return nil, fmt.Errorf("invalid TLS FD reset")
			}
			return &darwinTLSRecord{pid: uint32(pid), fd: int32(fd), fdMeta: 3}, nil
		}
		if len(fields) == 4 && fields[0] == "EDC_FD_BIND" {
			pid, e1 := strconv.ParseUint(fields[1], 10, 31)
			object, e2 := strconv.ParseUint(fields[2], 10, 64)
			fd, e3 := strconv.ParseUint(fields[3], 10, 31)
			if e1 != nil || e2 != nil || e3 != nil || pid == 0 || object == 0 {
				return nil, fmt.Errorf("invalid TLS FD binding")
			}
			return &darwinTLSRecord{pid: uint32(pid), object: object, fd: int32(fd), fdMeta: 1}, nil
		}
		if len(fields) != 10 || fields[0] != "EDC_FD_ADDR" {
			return nil, fmt.Errorf("invalid TLS FD metadata")
		}
		var values [9]uint64
		for i, field := range fields[1:] {
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid TLS FD address field")
			}
			values[i] = value
		}
		if values[0] == 0 || values[0] > 1<<31-1 || values[1] > 1<<31-1 || values[2] > 3 || (values[3] != 2 && values[3] != 30) || values[4] > 65535 {
			return nil, fmt.Errorf("invalid TLS FD address limits")
		}
		var bytes [16]byte
		for i, value := range values[5:] {
			if value > 1<<32-1 {
				return nil, fmt.Errorf("invalid TLS address word")
			}
			binary.LittleEndian.PutUint32(bytes[i*4:], uint32(value))
		}
		address := netip.AddrFrom16(bytes).Unmap()
		if values[3] == 2 {
			address = netip.AddrFrom4([4]byte(bytes[:4]))
		}
		return &darwinTLSRecord{pid: uint32(values[0]), fd: int32(values[1]), fdKind: uint8(values[2]), address: netip.AddrPortFrom(address, uint16(values[4])).String(), fdMeta: 2}, nil
	}
	if line == "EDC_GO_CONCURRENT" || strings.HasPrefix(line, "EDC_GO_CONCURRENT ") {
		return nil, fmt.Errorf("overlapping Go TLS calls on one connection and direction are unsupported: %s", line)
	}
	if strings.HasPrefix(line, "EDC_GO_WRITE_RESULT ") {
		if d.record != nil {
			return nil, fmt.Errorf("Go TLS write result interrupted a TLS record")
		}
		fields := strings.Fields(line)
		if len(fields) != 8 {
			return nil, fmt.Errorf("invalid Go TLS write result")
		}
		var values [6]uint64
		for i, field := range fields[1:7] {
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid Go TLS write result field")
			}
			values[i] = value
		}
		fd, err := strconv.ParseInt(fields[7], 10, 32)
		if err != nil || fd < -1 || values[0] == 0 || values[0] > 1<<31-1 || values[1] == 0 || values[2] == 0 || values[3] == 0 || values[5] > 1<<32-1 {
			return nil, fmt.Errorf("invalid Go TLS write result limits")
		}
		return &darwinTLSRecord{pid: uint32(values[0]), object: values[1], goroutine: values[2], frame: values[3], size: values[4], offset: uint32(values[5]), fd: int32(fd), sent: true, goPhase: 4}, nil
	}
	if strings.HasPrefix(line, "EDC_GO_") {
		if d.record != nil {
			return nil, fmt.Errorf("Go TLS control interrupted a TLS record")
		}
		fields := strings.Fields(line)
		if len(fields) != 6 || fields[0] != "EDC_GO_BEGIN" && fields[0] != "EDC_GO_END" {
			return nil, fmt.Errorf("invalid Go TLS control")
		}
		var values [5]uint64
		for i, field := range fields[1:] {
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid Go TLS control field")
			}
			values[i] = value
		}
		if values[0] == 0 || values[0] > 1<<31-1 || values[1] == 0 || values[2] > 1 || values[3] == 0 || values[4] == 0 {
			return nil, fmt.Errorf("invalid Go TLS control limits")
		}
		phase := uint8(1)
		if fields[0] == "EDC_GO_END" {
			phase = 2
		}
		return &darwinTLSRecord{pid: uint32(values[0]), object: values[1], sent: values[2] != 0, goroutine: values[3], frame: values[4], goPhase: phase}, nil
	}
	if strings.HasPrefix(line, "EDC_TLS ") {
		if d.record != nil {
			return nil, fmt.Errorf("incomplete DTrace TLS record")
		}
		fields := strings.SplitN(line, " ", 11)
		if len(fields) != 11 {
			return nil, fmt.Errorf("invalid DTrace TLS header")
		}
		var values [8]uint64
		for i, field := range fields[1:9] {
			n, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid DTrace TLS field")
			}
			values[i] = n
		}
		fd, err := strconv.ParseInt(fields[9], 10, 32)
		if err != nil || fd < -1 {
			return nil, fmt.Errorf("invalid DTrace TLS descriptor")
		}
		if values[0] == 0 || values[0] > 1<<31-1 || values[2] == 0 || values[3] > 1 || values[4] > 1 || values[5] > 1<<32-1 || values[6] > httpMessageMax || values[6] > values[7] || values[4] == 1 && (values[3] != 0 || values[5] != 0 || values[6] != 0 || values[7] != 0) {
			return nil, fmt.Errorf("DTrace TLS header exceeds capture limits")
		}
		d.captured = int(values[6])
		d.record = &darwinTLSRecord{pid: uint32(values[0]), wallNS: values[1], object: values[2], sent: values[3] != 0, closed: values[4] != 0, offset: uint32(values[5]), size: values[7], fd: int32(fd), process: fields[10], payload: make([]byte, 0, d.captured)}
		if writeSnapshot {
			if !d.record.sent || d.record.closed {
				d.record = nil
				return nil, fmt.Errorf("invalid Go TLS write snapshot direction")
			}
			d.record.goPhase = 3
		}
		return nil, nil
	}
	if line == "EDC_END" {
		if d.record == nil {
			return nil, fmt.Errorf("DTrace TLS record has no header")
		}
		record := d.record
		d.record = nil
		if len(record.payload) != d.captured {
			return nil, fmt.Errorf("DTrace TLS record has %d bytes, want %d", len(record.payload), d.captured)
		}
		return record, nil
	}
	if line == "EDC_FAULT" {
		return nil, fmt.Errorf("DTrace could not read a TLS buffer")
	}
	if d.record == nil || line == "" {
		return nil, nil
	}
	colon := strings.IndexByte(line, ':')
	if colon < 0 {
		return nil, nil
	}
	offset, err := strconv.ParseUint(strings.TrimSpace(line[:colon]), 16, 32)
	if err != nil || int(offset) != len(d.record.payload) {
		return nil, fmt.Errorf("invalid DTrace TLS payload offset")
	}
	count := min(16, d.captured-len(d.record.payload))
	fields := strings.Fields(line[colon+1:])
	if count == 0 || len(fields) < count {
		return nil, fmt.Errorf("invalid DTrace TLS payload row")
	}
	for _, field := range fields[:count] {
		if len(field) != 2 {
			return nil, fmt.Errorf("invalid DTrace TLS payload byte")
		}
		value, err := hex.DecodeString(field)
		if err != nil {
			return nil, fmt.Errorf("invalid DTrace TLS payload byte")
		}
		d.record.payload = append(d.record.payload, value[0])
	}
	return nil, nil
}

func darwinTLSModuleName(path string) (string, error) {
	at := strings.LastIndexByte(path, '/')
	name := path[at+1:]
	if name == "" {
		return "", fmt.Errorf("TLS target has an empty module name")
	}
	for _, letter := range name {
		if letter >= 'a' && letter <= 'z' || letter >= 'A' && letter <= 'Z' || letter >= '0' && letter <= '9' || strings.ContainsRune("._-+", letter) {
			continue
		}
		return "", fmt.Errorf("unsupported TLS module name %q", name)
	}
	return name, nil
}
func darwinTLSScript(targets []traceTLSTarget) (string, error) {
	var entries, returns, closes, sets, setReturns []string
	seen := map[string]bool{}
	var goTargets []traceTLSTarget
	secure := false
	for _, target := range targets {
		if target.goMachine != 0 {
			goTargets = append(goTargets, target)
			continue
		}
		modulePath := target.path
		if target.darwinModule != "" {
			modulePath = target.darwinModule
		}
		module, err := darwinTLSModuleName(modulePath)
		if err != nil {
			return "", err
		}
		for _, name := range target.symbols {
			prefix := "pid$target:" + module + ":" + name
			if seen[prefix] {
				continue
			}
			seen[prefix] = true
			switch name {
			case "SSL_read", "SSL_write", "SSL_read_ex", "SSL_write_ex", "SSLRead", "SSLWrite":
				if name == "SSLRead" || name == "SSLWrite" {
					secure = true
				}
				entries = append(entries, prefix+":entry")
				returns = append(returns, prefix+":return")
			case "SSL_free", "SSLClose", "SSLDisposeContext":
				closes = append(closes, prefix+":entry")
			case "SSL_set_fd":
				sets = append(sets, prefix+":entry")
				setReturns = append(setReturns, prefix+":return")
			default:
				return "", fmt.Errorf("unsupported macOS TLS function %q", name)
			}
		}
	}
	if len(goTargets) == 0 && (len(entries) == 0 || len(closes) == 0) {
		return "", fmt.Errorf("TLS target needs SSL read/write functions and SSL_free")
	}
	entry, ret := strings.Join(entries, ",\n"), strings.Join(returns, ",\n")
	var script strings.Builder
	dynamicSize := "2m"
	if len(goTargets) > 0 {
		dynamicSize = "16m"
	}
	script.WriteString("#pragma D option dynvarsize=" + dynamicSize + "\n")
	script.WriteString(`#pragma D option quiet
#pragma D option rawbytes
#pragma D option temporal
#pragma D option bufsize=2m
#pragma D option switchrate=20hz

self uint64_t edc_depth;
self uint64_t edc_object;
self uint64_t edc_buffer;
self uint64_t edc_requested;
self uint64_t edc_out;
self uint64_t edc_secure;
self uint64_t edc_sent;
self uint64_t edc_wall;
self uint64_t edc_actual;
self uint64_t edc_captured;
self uint64_t edc_set_object;
self uint64_t edc_set_fd;
uint64_t edc_fd[pid_t,uint64_t];
uint64_t edc_offset[pid_t,uint64_t,uint64_t];

`)
	// macOS requires kernel privileges for copyin; sized user references need only process access.
	for size := 16; size <= httpMessageMax; size *= 2 {
		fmt.Fprintf(&script, "typedef struct { uint8_t bytes[%d]; } edc_tls_bytes_%d;\n", size, size)
	}
	script.WriteString("\nBEGIN { printf(\"EDC_READY\\n\"); }\n")
	if len(entries) > 0 {
		fmt.Fprintf(&script, `
%s
{
 self->edc_object = self->edc_depth ? self->edc_object : arg0;
 self->edc_buffer = self->edc_depth ? self->edc_buffer : arg1;
 self->edc_requested = self->edc_depth ? self->edc_requested : arg2;
 self->edc_secure = self->edc_depth ? self->edc_secure :
  (probefunc == "SSLRead" || probefunc == "SSLWrite");
 self->edc_out = self->edc_depth ? self->edc_out :
  ((probefunc == "SSL_read_ex" || probefunc == "SSL_write_ex" || self->edc_secure) ? arg3 : 0);
 self->edc_sent = self->edc_depth ? self->edc_sent :
  (probefunc == "SSL_write" || probefunc == "SSL_write_ex" || probefunc == "SSLWrite");
 self->edc_wall = self->edc_depth ? self->edc_wall : walltimestamp;
 self->edc_depth++;
}

`, entry)
		if len(sets) > 0 {
			fmt.Fprintf(&script, `
%s
{ self->edc_set_object=arg0; self->edc_set_fd=arg1; }
%s
/self->edc_set_object && (int)arg1 == 1/
{ edc_fd[pid,self->edc_set_object]=self->edc_set_fd+1; }
%s
{ self->edc_set_object=0; self->edc_set_fd=0; }
`, strings.Join(sets, ",\n"), strings.Join(setReturns, ",\n"), strings.Join(setReturns, ",\n"))
		}
		fmt.Fprintf(&script, `
%s
/self->edc_depth == 1/
{
 self->edc_actual = self->edc_secure ?
  (((int)arg1 == 0 || (int)arg1 == -9803 || (int)arg1 == -9805 || (int)arg1 == -9816) && self->edc_out ?
   *(userland uint64_t *)self->edc_out : 0) : self->edc_out ?
  ((int)arg1 == 1 ? *(userland uint64_t *)self->edc_out : 0) :
  ((int)arg1 > 0 ? (uint32_t)arg1 : 0);
 self->edc_captured = self->edc_actual > %d ? %d : self->edc_actual;
}
`, ret, httpMessageMax, httpMessageMax)
		script.WriteString(darwinTLSDataScript(ret, "self->edc_depth == 1", "self->"))

		fmt.Fprintf(&script, `
%s
/self->edc_depth == 1 && self->edc_actual > 0 && self->edc_actual <= self->edc_requested/
{ edc_offset[pid,self->edc_object,self->edc_sent] += self->edc_actual; }

%s
/self->edc_depth/
{
 self->edc_depth--;
 self->edc_object = self->edc_depth ? self->edc_object : 0;
 self->edc_buffer = self->edc_depth ? self->edc_buffer : 0;
 self->edc_requested = self->edc_depth ? self->edc_requested : 0;
 self->edc_out = self->edc_depth ? self->edc_out : 0;
 self->edc_secure = self->edc_depth ? self->edc_secure : 0;
 self->edc_sent = self->edc_depth ? self->edc_sent : 0;
 self->edc_wall = self->edc_depth ? self->edc_wall : 0;
 self->edc_actual = self->edc_depth ? self->edc_actual : 0;
 self->edc_captured = self->edc_depth ? self->edc_captured : 0;
}

%s
/arg0 != 0/
{
 printf("EDC_TLS %%d %%llu %%llu 0 1 0 0 0 -1 %%s\nEDC_END\n",pid,(uint64_t)walltimestamp,(uint64_t)arg0,execname);
 edc_fd[pid,arg0]=0;
 edc_offset[pid,arg0,0]=0;
 edc_offset[pid,arg0,1]=0;
}
`, ret, ret, strings.Join(closes, ",\n"))
	}
	if secure {
		script.WriteString(darwinSecureTransportFDScript)
	}
	if secure || len(goTargets) > 0 {
		script.WriteString(darwinTLSFDAddressScript)
	}
	if len(goTargets) > 0 {
		body, err := darwinGoTLSBinaryScript(goTargets)
		if err != nil {
			return "", err
		}
		script.WriteString(body)
	}
	script.WriteString("ERROR { printf(\"EDC_FAULT\\n\"); }\n")
	return script.String(), nil
}

func darwinTLSDataScript(probes, guard, state string) string {
	var script strings.Builder
	for size := httpMessageMax; size >= 1; size /= 2 {
		fmt.Fprintf(&script, `
%s
/%s && self->edc_actual <= self->edc_requested && (self->edc_captured & %d) != 0/
{
 this->offset = self->edc_captured & ~((uint64_t)%d);
 printf("EDC_TLS %%d %%llu %%llu %%d 0 %%u %d %%llu %%d %%s\n",
  pid,(self->edc_sent ? self->edc_wall : (uint64_t)walltimestamp),self->edc_object,self->edc_sent,
  (uint32_t)(edc_offset[pid,self->edc_object,self->edc_sent]+this->offset),
  (self->edc_actual > %d ? self->edc_actual : (uint64_t)%d),
  (int)edc_fd[pid,self->edc_object]-1,execname);
`, probes, guard, size, 2*size-1, size, httpMessageMax, size)
		if size >= 16 {
			fmt.Fprintf(&script, " trace(*(userland edc_tls_bytes_%d *)(self->edc_buffer+this->offset));\n", size)
		} else {
			script.WriteString(` printf("    0:`)
			for i := 0; i < size; i++ {
				script.WriteString(" %02x")
			}
			script.WriteString(`\n"`)
			for i := 0; i < size; i++ {
				fmt.Fprintf(&script, ",(uint32_t)*(userland uint8_t *)(self->edc_buffer+this->offset+%d)", i)
			}
			script.WriteString(");\n")
		}
		script.WriteString(` printf("\nEDC_END\n");
}
`)
	}

	return strings.ReplaceAll(script.String(), "self->", state)
}
