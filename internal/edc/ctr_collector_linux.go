//go:build linux

package edc

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func RunCtrCollector(input []byte) ([]byte, int) {
	if len(input) == 0 || len(input) > ctrCollectorInputLimit {
		return nil, 2
	}
	request, err := decodeCtrCollectorRequest(input)
	if err != nil {
		return nil, 2
	}
	response := collectLinuxCtr(request)
	data, err := marshalCtrCollectorResponse(response)
	if err != nil {
		return nil, 1
	}
	return append(data, '\n'), 0
}

func decodeCtrCollectorRequest(data []byte) (ctrCollectorRequest, error) {
	var request ctrCollectorRequest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return ctrCollectorRequest{}, err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err == nil {
		return ctrCollectorRequest{}, fmt.Errorf("multiple collector requests")
	} else if !errors.Is(err, io.EOF) {
		return ctrCollectorRequest{}, err
	}
	if request.ProtocolVersion != ctrCollectorProtocolVersion || request.RequestID == "" || !validCtrContainerID(request.TargetID) || request.TargetPID < 0 || request.DeadlineMS < 1 || request.DeadlineMS > 5000 {
		return ctrCollectorRequest{}, fmt.Errorf("invalid collector request")
	}
	return request, nil
}

func collectLinuxCtr(request ctrCollectorRequest) ctrCollectorResponse {
	deadline := time.Now().Add(time.Duration(request.DeadlineMS) * time.Millisecond)
	response := ctrCollectorResponse{RequestID: request.RequestID, TargetID: request.TargetID, TargetPID: request.TargetPID, ProtocolVersion: ctrCollectorProtocolVersion}
	base := ""
	var start uint64
	var cgroupPath string
	var err error
	if request.TargetPID > 0 {
		base = filepath.Join("/proc", strconv.Itoa(request.TargetPID))
		start, err = readCtrProcStart(base)
		if err != nil {
			return ctrCollectorWholeUnavailable(response, "target process identity: "+err.Error())
		}
		membership, readErr := readCtrCollectorBounded(filepath.Join(base, "cgroup"), 4096)
		if readErr != nil {
			return ctrCollectorWholeUnavailable(response, "target cgroup: "+readErr.Error())
		}
		cgroupPath, err = linuxUnifiedCgroupPath(string(membership))
	} else {
		response.FDStatus = collectorUnavailable("target container is not running")
		response.ListenStatus = collectorUnavailable("target container is not running")
		response.OOMStatus = collectorUnavailable("target container cgroup counters are unavailable while stopped")
		response.LogBytes, response.LogStatus = ctrCollectorLog(request.LogAvailable)
		return response
	}
	if err != nil {
		return ctrCollectorWholeUnavailable(response, err.Error())
	}
	if !validCtrContainerID(request.TargetID) || !ctrCgroupMatchesID(cgroupPath, request.TargetID) {
		return ctrCollectorWholeUnavailable(response, "target cgroup does not contain the full container ID")
	}
	groupPath := filepath.Join("/host/cgroup", strings.TrimPrefix(cgroupPath, "/"))
	if filepath.Clean(groupPath) != groupPath || !strings.HasPrefix(groupPath, "/host/cgroup/") {
		return ctrCollectorWholeUnavailable(response, "target cgroup path escapes the bound cgroup root")
	}
	groupInfo, err := os.Stat(groupPath)
	if err != nil {
		return ctrCollectorWholeUnavailable(response, "target cgroup directory: "+err.Error())
	}
	if _, ok := groupInfo.Sys().(*syscall.Stat_t); !ok {
		return ctrCollectorWholeUnavailable(response, "target cgroup identity is unavailable")
	}
	if request.TargetPID > 0 {
		processes, partial, processErr := ctrCollectorProcesses(groupPath, deadline)
		if processErr != nil {
			return ctrCollectorWholeUnavailable(response, processErr.Error())
		}
		response.FDs, response.FDStatus = ctrCollectorFDs(processes, partial, groupPath, deadline)
		response.Listeners, response.ListenStatus = ctrCollectorListeners(processes, partial, groupPath, deadline)
	} else {
		response.FDStatus = collectorUnavailable("target container is not running")
		response.ListenStatus = collectorUnavailable("target container is not running")
	}
	if time.Now().Before(deadline) {
		response.OOM, response.OOMKill, response.OOMStatus = ctrCollectorOOM(groupPath)
	} else {
		response.OOMStatus = collectorUnavailable("helper deadline exceeded")
	}
	if time.Now().Before(deadline) {
		response.LogBytes, response.LogStatus = ctrCollectorLog(request.LogAvailable)
	} else {
		response.LogStatus = collectorUnavailable("helper deadline exceeded")
	}
	end, endErr := start, error(nil)
	if request.TargetPID > 0 {
		end, endErr = readCtrProcStart(base)
	}
	endGroup, groupErr := os.Stat(groupPath)
	if endErr != nil || end != start || groupErr != nil || !sameCtrCollectorFile(groupInfo, endGroup) {
		return ctrCollectorWholeUnavailable(response, "target process or cgroup identity changed during collection")
	}
	return response
}

func ctrCollectorWholeUnavailable(response ctrCollectorResponse, reason string) ctrCollectorResponse {
	status := collectorUnavailable(reason)
	response.FDStatus, response.ListenStatus, response.OOMStatus, response.LogStatus = status, status, status, status
	response.FDs = nil
	response.Listeners = nil
	response.OOM, response.OOMKill, response.LogBytes = nil, nil, nil
	return response
}

func ctrCgroupMatchesID(path, id string) bool {
	for _, part := range strings.Split(path, "/") {
		if part == id || part == "docker-"+id+".scope" || part == id+".scope" {
			return true
		}
	}
	return false
}

func readCtrProcStart(base string) (uint64, error) {
	data, err := readCtrCollectorBounded(filepath.Join(base, "stat"), 4096)
	if err != nil {
		return 0, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 || end+1 >= len(data) {
		return 0, fmt.Errorf("invalid proc stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 {
		return 0, fmt.Errorf("proc stat lacks start time")
	}
	return strconv.ParseUint(fields[19], 10, 64)
}

func ctrCollectorPIDInCgroup(pid int, root string) bool {
	path, err := ctrCollectorPIDCgroup(pid)
	return err == nil && ctrCollectorCgroupWithin(path, root)
}

func ctrCollectorPIDCgroup(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid process ID")
	}
	membership, err := readCtrCollectorBounded(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"), 4096)
	if err != nil {
		return "", err
	}
	path, err := linuxUnifiedCgroupPath(string(membership))
	return path, err
}

func ctrCollectorCgroupWithin(path, root string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Clean(root) != root || root != "/host/cgroup" && !strings.HasPrefix(root, "/host/cgroup/") {
		return false
	}
	relative, err := filepath.Rel("/host/cgroup", root)
	if err != nil {
		return false
	}
	wanted := "/"
	if relative != "." {
		wanted = "/" + relative
	}
	return path == wanted || strings.HasPrefix(path, wanted+"/")
}

func ctrCollectorProcessStable(startBefore, startAfter uint64, cgroupPath, root string) bool {
	return startBefore == startAfter && ctrCollectorCgroupWithin(cgroupPath, root)
}

func ctrCollectorProcesses(root string, deadline time.Time) ([]int, bool, error) {
	processes := map[int]struct{}{}
	partial := false
	queue := []string{root}
	groups := 0
	for len(queue) > 0 {
		if time.Now().After(deadline) {
			partial = true
			break
		}
		path := queue[0]
		queue[0] = ""
		queue = queue[1:]
		groups++
		if groups > ctrCollectorProcessLimit {
			partial = true
			break
		}
		data, readErr := readCtrCollectorBounded(filepath.Join(path, "cgroup.procs"), ctrCollectorProcessLimit*12)
		if readErr != nil {
			partial = true
		}
		for _, line := range strings.Fields(string(data)) {
			if time.Now().After(deadline) {
				partial = true
				break
			}
			pid, parseErr := strconv.Atoi(line)
			if parseErr != nil || pid <= 0 {
				partial = true
				continue
			}
			if !ctrCollectorPIDInCgroup(pid, root) {
				partial = true
				continue
			}
			if _, exists := processes[pid]; !exists && len(processes) >= ctrCollectorProcessLimit {
				partial = true
				break
			}
			processes[pid] = struct{}{}
		}
		remaining := ctrCollectorProcessLimit - groups
		if remaining <= 0 || time.Now().After(deadline) {
			partial = true
			continue
		}
		directory, openErr := os.Open(path)
		if openErr != nil {
			partial = true
			continue
		}
		children, readErr := directory.ReadDir(remaining + 1)
		directory.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			partial = true
			continue
		}
		if len(children) == remaining+1 {
			partial = true
		}
		for _, child := range children {
			if !child.IsDir() {
				continue
			}
			if groups+len(queue) >= ctrCollectorProcessLimit {
				partial = true
				break
			}
			queue = append(queue, filepath.Join(path, child.Name()))
		}
	}
	if len(processes) == 0 {
		return nil, partial, fmt.Errorf("target cgroup has no readable processes")
	}
	ids := make([]int, 0, len(processes))
	for pid := range processes {
		ids = append(ids, pid)
	}
	sort.Ints(ids)
	return ids, partial, nil
}

func ctrCollectorFDs(pids []int, partial bool, root string, deadline time.Time) ([]ctrCollectorFD, ctrCollectorMetricStatus) {
	rows := make([]ctrCollectorFD, 0, len(pids))
	complete := !partial
	count := 0
	for _, pid := range pids {
		if time.Now().After(deadline) {
			complete = false
			break
		}
		base := filepath.Join("/proc", strconv.Itoa(pid))
		start, startErr := readCtrProcStart(base)
		if startErr != nil || !ctrCollectorPIDInCgroup(pid, root) {
			if startErr == nil {
				startErr = fmt.Errorf("process is outside the target cgroup")
			}
			rows = append(rows, ctrCollectorFD{PID: pid, Status: linuxLimitError(startErr, false).Status, Reason: startErr.Error()})
			complete = false
			continue
		}
		directory, err := os.Open(filepath.Join(base, "fd"))
		if err != nil {
			rows = append(rows, ctrCollectorFD{PID: pid, Status: linuxLimitError(err, false).Status, Reason: err.Error()})
			complete = false
			continue
		}
		fd, err := directory.Readdirnames(min(ctrCollectorFDLimit+1, ctrCollectorFDLimit-count+1))
		directory.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			complete = false
			continue
		}
		count += len(fd)
		if count > ctrCollectorFDLimit {
			complete = false
			rows = append(rows, ctrCollectorFD{PID: pid, Status: "partial", Reason: "process FD limit exceeded"})
			break
		}
		limit := ctrCollectorFDLimitForPID(base)
		row := ctrCollectorFD{PID: pid, Status: limit.Status, Reason: limit.Reason, Unlimited: limit.Unlimited}
		if limit.Used != nil {
			value := uint64(*limit.Used)
			row.Used = &value
		}
		row.Soft = limit.Soft
		end, endErr := readCtrProcStart(base)
		memberPath, memberErr := ctrCollectorPIDCgroup(pid)
		if endErr != nil || memberErr != nil || !ctrCollectorProcessStable(start, end, memberPath, root) {
			row.Used, row.Soft, row.Unlimited = nil, nil, false
			row.Status, row.Reason = "unavailable", "process identity changed during FD collection"
		}
		if row.Status != "available" {
			complete = false
		}
		rows = append(rows, row)
	}
	status := ctrCollectorMetricStatus{Status: "available"}
	if !complete {
		status = ctrCollectorMetricStatus{Status: "partial", Reason: "one or more process FD values are incomplete"}
	}
	return rows, status
}

func ctrCollectorOOM(groupPath string) (*uint64, *uint64, ctrCollectorMetricStatus) {
	data, err := readCtrCollectorBounded(filepath.Join(groupPath, "memory.events"), 4096)
	if err != nil {
		status := linuxLimitError(err, true)
		return nil, nil, ctrCollectorMetricStatus{Status: status.Status, Reason: status.Reason}
	}
	values := map[string]uint64{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, nil, ctrCollectorMetricStatus{Status: "error", Reason: "malformed memory.events row"}
		}
		if _, exists := values[fields[0]]; exists {
			return nil, nil, ctrCollectorMetricStatus{Status: "error", Reason: "duplicate memory.events counter"}
		}
		value, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr != nil {
			return nil, nil, ctrCollectorMetricStatus{Status: "error", Reason: "invalid memory.events counter"}
		}
		values[fields[0]] = value
	}
	oom, oomOK := values["oom"]
	kill, killOK := values["oom_kill"]
	if !oomOK || !killOK {
		return nil, nil, ctrCollectorMetricStatus{Status: "unsupported", Reason: "memory.events lacks oom counters"}
	}
	return &oom, &kill, ctrCollectorMetricStatus{Status: "available"}
}

func ctrCollectorFDLimitForPID(base string) topFDLimit {
	directory, err := os.Open(filepath.Join(base, "fd"))
	if err != nil {
		return topFDLimit{topLimitStatus: linuxLimitError(err, false)}
	}
	names, err := directory.Readdirnames(ctrCollectorFDLimit + 1)
	directory.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return topFDLimit{topLimitStatus: linuxLimitError(err, false)}
	}
	if len(names) > ctrCollectorFDLimit {
		return topFDLimit{topLimitStatus: topLimitStatus{Status: "partial", Reason: "process FD limit exceeded"}}
	}
	data, err := readCtrCollectorBounded(filepath.Join(base, "limits"), 16<<10)
	if err != nil {
		return topFDLimit{topLimitStatus: linuxLimitError(err, false)}
	}
	soft, unlimited, err := parseLinuxFDSoftLimit(string(data))
	if err != nil {
		return topFDLimit{topLimitStatus: topLimitStatus{Status: "error", Reason: err.Error()}}
	}
	used := len(names)
	return topFDLimit{topLimitStatus: topLimitStatus{Status: "available"}, Used: &used, Soft: soft, Unlimited: unlimited}
}

func readCtrCollectorBounded(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", filepath.Base(path), limit)
	}
	return data, nil
}

func ctrCollectorLog(available bool) (*uint64, ctrCollectorMetricStatus) {
	if !available {
		return nil, collectorUnavailable("container log path is not eligible for read-only binding")
	}
	fd, err := unix.Open("/input/log", unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, collectorUnavailable("container log file: " + err.Error())
	}
	defer unix.Close(fd)
	var stat syscall.Stat_t
	if err = syscall.Fstat(fd, &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Size < 0 {
		return nil, collectorUnavailable("container log path is not a regular file")
	}
	size := uint64(stat.Size)
	return &size, ctrCollectorMetricStatus{Status: "available"}
}

func sameCtrCollectorFile(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return false
	}
	left, leftOK := a.Sys().(*syscall.Stat_t)
	right, rightOK := b.Sys().(*syscall.Stat_t)
	return leftOK && rightOK && left.Dev == right.Dev && left.Ino == right.Ino
}

func ctrCollectorListeners(pids []int, processPartial bool, root string, deadline time.Time) ([]ctrCollectorListen, ctrCollectorMetricStatus) {
	owned := map[uint64]struct{}{}
	netns := map[uint64]ctrCollectorNetnsOwner{}
	partial := processPartial
	fds := 0
	for _, pid := range pids {
		if time.Now().After(deadline) {
			partial = true
			break
		}
		base := filepath.Join("/proc", strconv.Itoa(pid))
		start, startErr := readCtrProcStart(base)
		memberPath, memberErr := ctrCollectorPIDCgroup(pid)
		if startErr != nil || memberErr != nil || !ctrCollectorCgroupWithin(memberPath, root) {
			partial = true
			continue
		}
		directory, err := os.Open(filepath.Join(base, "fd"))
		if err != nil {
			partial = true
			continue
		}
		entries, err := directory.Readdirnames(min(ctrCollectorFDLimit+1, ctrCollectorFDLimit-fds+1))
		directory.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			partial = true
			continue
		}
		fds += len(entries)
		if fds > ctrCollectorFDLimit {
			return nil, ctrCollectorMetricStatus{Status: "partial", Reason: "process FD limit exceeded"}
		}
		for _, entry := range entries {
			target, err := os.Readlink(filepath.Join(base, "fd", entry))
			if err != nil {
				partial = true
				continue
			}
			if inode, ok := parseCtrSocketInode(target); ok {
				owned[inode] = struct{}{}
			}
		}
		info, err := os.Stat(filepath.Join(base, "ns/net"))
		if err != nil {
			partial = true
			continue
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			netns[stat.Ino] = ctrCollectorNetnsOwner{pid: pid, start: start}
		}
	}
	seen := map[uint64]struct{}{}
	listeners := make([]ctrCollectorListen, 0)
	for _, owner := range netns {
		if time.Now().After(deadline) {
			partial = true
			break
		}
		pid := owner.pid
		base := filepath.Join("/proc", strconv.Itoa(pid))
		if start, err := readCtrProcStart(base); err != nil || start != owner.start || !ctrCollectorPIDInCgroup(pid, root) {
			partial = true
			continue
		}
		var namespaceRows []ctrCollectorListen
		namespaceInodes := map[uint64]struct{}{}
		for _, table := range []struct{ name, proto string }{{"tcp", "tcp"}, {"tcp6", "tcp"}, {"udp", "udp"}, {"udp6", "udp"}} {
			if time.Now().After(deadline) {
				partial = true
				break
			}
			file := filepath.Join("/proc", strconv.Itoa(pid), "net", table.name)
			rows, err := parseCtrProcNetTable(file, table.proto, deadline)
			if err != nil {
				partial = true
				continue
			}
			for _, row := range rows {
				if _, ok := owned[row.Inode]; !ok {
					continue
				}
				if _, ok := seen[row.Inode]; ok {
					continue
				}
				if _, ok := namespaceInodes[row.Inode]; ok {
					continue
				}
				if len(seen)+len(namespaceInodes) >= ctrCollectorSocketLimit {
					return listeners, ctrCollectorMetricStatus{Status: "partial", Reason: "socket limit exceeded"}
				}
				namespaceInodes[row.Inode] = struct{}{}
				namespaceRows = append(namespaceRows, row)
			}
		}
		if start, err := readCtrProcStart(base); err != nil || start != owner.start || !ctrCollectorPIDInCgroup(pid, root) {
			partial = true
			continue
		}
		listeners = append(listeners, namespaceRows...)
		for inode := range namespaceInodes {
			seen[inode] = struct{}{}
		}
	}
	sort.Slice(listeners, func(i, j int) bool {
		if listeners[i].Proto != listeners[j].Proto {
			return listeners[i].Proto < listeners[j].Proto
		}
		if listeners[i].Port != listeners[j].Port {
			return listeners[i].Port < listeners[j].Port
		}
		return listeners[i].Address < listeners[j].Address
	})
	if partial {
		return listeners, ctrCollectorMetricStatus{Status: "partial", Reason: "one or more process or network namespace reads failed"}
	}
	return listeners, ctrCollectorMetricStatus{Status: "available"}
}

type ctrCollectorNetnsOwner struct {
	pid   int
	start uint64
}

func parseCtrSocketInode(value string) (uint64, bool) {
	if !strings.HasPrefix(value, "socket:[") || !strings.HasSuffix(value, "]") {
		return 0, false
	}
	inode, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(value, "socket:["), "]"), 10, 64)
	return inode, err == nil && inode > 0
}

func parseCtrProcNetTable(path, proto string, deadline time.Time) ([]ctrCollectorListen, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var rows []ctrCollectorListen
	line := 0
	for scanner.Scan() {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("helper deadline exceeded")
		}
		line++
		if line == 1 {
			continue
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			return nil, fmt.Errorf("malformed proc net row")
		}
		state, err := strconv.ParseUint(fields[3], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("malformed proc net state")
		}
		if proto == "tcp" && state != 0x0a || proto == "udp" && state != 0x07 {
			continue
		}
		address, port, err := parseCtrProcAddress(fields[1])
		if err != nil {
			return nil, err
		}
		if port == 0 {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("malformed proc net inode")
		}
		rows = append(rows, ctrCollectorListen{Proto: proto, Address: address, Port: port, Inode: inode})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}

func parseCtrProcAddress(value string) (string, int, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("malformed proc net address")
	}
	portValue, err := strconv.ParseUint(parts[1], 16, 16)
	if err != nil {
		return "", 0, fmt.Errorf("malformed proc net port")
	}
	decoded, err := hex.DecodeString(parts[0])
	if err != nil || len(decoded) != 4 && len(decoded) != 16 {
		return "", 0, fmt.Errorf("malformed proc net IP address")
	}
	if len(decoded) == 4 {
		for i, j := 0, len(decoded)-1; i < j; i, j = i+1, j-1 {
			decoded[i], decoded[j] = decoded[j], decoded[i]
		}
	} else {
		for i := 0; i < len(decoded); i += 4 {
			decoded[i], decoded[i+3] = decoded[i+3], decoded[i]
			decoded[i+1], decoded[i+2] = decoded[i+2], decoded[i+1]
		}
	}
	addr, ok := netip.AddrFromSlice(decoded)
	if !ok {
		return "", 0, fmt.Errorf("invalid proc net IP address")
	}
	return addr.String(), int(portValue), nil
}
