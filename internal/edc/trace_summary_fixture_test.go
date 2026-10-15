package edc

import (
	"os"
	"time"
)

// traceSummaryFixture는 요약 printer 하나와 그 report다. print는 os.Stdout에 쓴다. 색 test와 평문 test가 같은
// report를 그린다.
type traceSummaryFixture struct {
	name  string
	print func()
}

func traceSummaryFixtures() []traceSummaryFixture {
	ms := func(value float64) *float64 { return &value }
	u64 := func(value uint64) *uint64 { return &value }
	traffic := traceTraffic{TXBytes: 2048, RXBytes: 512, TotalBytes: 2560, BytesPerSecond: 1280, BitsPerSecond: 10240, MegabitsPerSecond: 0.01}
	connect, reset, rtt := int64(12), true, uint32(840)
	tcp := tcpTraceReport{DurationMS: 2000, Attempts: 3, Established: 2, Incomplete: 1, Retransmissions: u64(4), Resets: new(int), LostEvents: 2, traceTraffic: traffic,
		Connections: []tcpTraceConnection{
			{Process: "curl", Destination: "10.0.0.5:443", Hostname: "api.example", Result: "established", ConnectMS: &connect, Retransmissions: u64(4), Reset: &reset, RTTUS: &rtt, traceTraffic: traffic},
			{Process: "nc", Destination: "10.0.0.9:22", Result: "refused", Retransmissions: u64(0), Reset: new(bool)},
		},
		ConnectionsOmitted: 7,
		groups: []tcpTraceGroupRow{
			{process: "curl", peer: "10.0.0.5:443", hostname: "api.example", connections: 2, established: 2, retransmissions: 4, resets: 1, connectTotalMS: 24, connectCount: 2, rttTotalUS: 1680, rttCount: 2, traceTraffic: traffic},
			{process: "nc", peer: "10.0.0.9:22", connections: 1, incomplete: 1},
		}}
	udp := udpTraceReport{DurationMS: 1500, Datagrams: 4, Sent: 3, Received: 1, traceTraffic: traffic,
		Flows:  []udpTraceFlow{{Process: "dig", Destination: "1.1.1.1:53", Hostname: "one.one", Sent: 3, Received: 1, traceTraffic: traffic}},
		groups: []udpTraceGroupRow{{process: "dig", peer: "1.1.1.1:53", sent: 3, received: 1, traceTraffic: traffic}}}
	group := traceGroupReport{Protocol: "tcp", GroupBy: traceGroupByTarget, DurationMS: 3000, Events: 9, Rate: 3, LostEvents: 1, traceTraffic: traffic, Groups: []traceGroupSummary{
		{Group: "api.example", Destinations: []string{"10.0.0.5:443"}, Events: 6, Rate: 2, Connect: 2, Retransmissions: u64(3), Resets: u64(1), LastEvent: "tcp_reset", traceTraffic: traffic},
		{Group: "10.0.0.9", Destinations: []string{"10.0.0.9"}, Events: 3, Rate: 1, Connect: 1, Retransmissions: u64(0), Resets: u64(0), LastEvent: "connect"},
	}}
	arpGroup := traceGroupReport{Protocol: "arp", GroupBy: traceGroupByEvent, DurationMS: 2000, Events: 2, Rate: 1, LostEvents: 3, Groups: []traceGroupSummary{
		{Group: "arp_mac_change", Events: 1, Rate: 0.5, LastEvent: "arp_mac_change", Neighbor: &traceNeighborCounts{MACChanges: 1}},
		{Group: "arp_reachable", Events: 1, Rate: 0.5, LastEvent: "arp_reachable", Neighbor: &traceNeighborCounts{}},
	}}
	neighbor := neighborTraceReport{protocol: "arp", DurationMS: 2000, Events: 3, LostEvents: 1, MACChanges: 1, Failures: 0, Neighbors: []neighborTraceRow{
		{Interface: "en0", IP: "192.168.0.1", MAC: "aa:bb:cc:dd:ee:ff", State: "reachable", Events: 2, traceNeighborCounts: traceNeighborCounts{MACs: []string{"aa", "bb"}, MACChanges: 1}},
		{Interface: "en0", IP: "192.168.0.7", Events: 1},
	}}
	dns := dnsTraceReport{DurationMS: 2000, LostEvents: 0, traceDNSCounts: traceDNSCounts{Queries: 4, Answers: 3, Errors: 1, Unanswered: 1, LatencyAvgMS: ms(3.2), LatencyMaxMS: ms(9.5)},
		Names: []dnsTraceName{{Name: "api.example", Type: "A", Processes: []string{"curl"}, Results: map[string]uint64{"noerror": 2, "nxdomain": 1}, traceDNSCounts: traceDNSCounts{Queries: 3, Answers: 3, LatencyAvgMS: ms(2.5), LatencyMaxMS: ms(4)}}}}
	httpCounts := traceHTTPCounts{Requests: 3, Responses: 3, ClientErrors: 1, ServerErrors: 1, LatencyAvgMS: ms(12.5), LatencyMaxMS: ms(30)}
	http := httpTraceReport{DurationMS: 4000, LostEvents: 2, traceHTTPCounts: httpCounts, Client: &httpCounts, Server: &traceHTTPCounts{Requests: 1, Responses: 1}, TLSUnmapped: 1,
		Paths:          []httpTraceRow{{Side: "client", Method: "GET", Host: "api.example", Path: "/v1/users", Processes: []string{"curl"}, Statuses: map[string]uint64{"200": 1, "404": 1, "503": 1}, traceHTTPCounts: httpCounts}},
		TLSConnections: 1, TLSAlerts: map[string]uint64{"fatal handshake_failure": 1},
		TLS: []httpTLSRow{{Side: "client", Host: "secure.example", ALPN: []string{"h2"}, Connections: 1, Versions: map[string]uint64{"TLS 1.3": 1}, Alerts: 1, Processes: []string{"curl"}}}}
	mysql := mysqlTraceReport{DurationMS: 2000, LostEvents: 0, traceMySQLCounts: traceMySQLCounts{Commands: 5, Responses: 5, Errors: 1, LatencyAvgMS: ms(1.5), LatencyMaxMS: ms(4), Connections: 1},
		Statements: []mysqlTraceRow{{Command: "COM_QUERY", Shape: "SELECT * FROM users WHERE id = ?", Processes: []string{"app"}, traceMySQLCounts: traceMySQLCounts{Commands: 5, Errors: 1, LatencyAvgMS: ms(1.5), LatencyMaxMS: ms(4)}}}}
	postgresCounts := tracePostgresCounts{Commands: 2, Responses: 1, Unanswered: 1, LatencyAvgMS: ms(0.8), LatencyMaxMS: ms(0.8), Connections: 1}
	postgres := postgresTraceReport{DurationMS: 2000, tracePostgresCounts: postgresCounts, Client: &postgresCounts, Server: &postgresCounts,
		Statements: []postgresTraceRow{{Side: "client", Command: "Query", Shape: "SELECT 1", Processes: []string{"psql"}, tracePostgresCounts: postgresCounts}}}
	redis := redisTraceReport{DurationMS: 2000, traceRedisCounts: traceRedisCounts{Commands: 3, Replies: 3, Errors: 0, LatencyAvgMS: ms(0.2), LatencyMaxMS: ms(0.4)},
		Rows: []redisTraceRow{{Command: "GET", Processes: []string{"app"}, traceRedisCounts: traceRedisCounts{Commands: 3, LatencyAvgMS: ms(0.2), LatencyMaxMS: ms(0.4)}}}}
	clickhouse := clickHouseTraceReport{DurationMS: 2000, traceClickHouseCounts: traceClickHouseCounts{Queries: 2, Responses: 2, Errors: 1, Connections: 1},
		Statements: []clickHouseTraceRow{
			{Interface: "native", Command: "Query", Shape: "SELECT count() FROM t", Processes: []string{"app"}, traceClickHouseCounts: traceClickHouseCounts{Queries: 1, Errors: 1}},
			{Interface: "http", Command: "Query", Shape: "SELECT 1", Processes: []string{"curl"}, traceClickHouseCounts: traceClickHouseCounts{Queries: 1}},
		}}
	socket := socketTraceReport{Path: "/run/app.sock", DurationMS: 2000, Events: 4, TXBytes: 100, RXBytes: 2000, Failures: 1, Rows: []socketTraceRow{
		{Process: "app", Peer: "nginx", Side: "server", Events: 4, Accepts: 1, Sends: 1, TXBytes: 100, Recvs: 1, RXBytes: 2000, Failures: 1, Closes: 1},
	}}
	drop := dropTraceReport{DurationMS: 2000, Drops: 3, Events: 3, Sampled: 0, LostEvents: 0, ListenOverflows: u64(0), ListenDrops: u64(2),
		Reasons: []dropReasonTotal{{Reason: "NO_SOCKET", Drops: 3}},
		Rows:    []dropTraceRow{{Reason: "NO_SOCKET", Location: "tcp_v4_rcv", Events: 3, Bytes: 180, Processes: []string{"-"}, Example: "10.0.0.1:5000 > 10.0.0.2:81"}}}
	syscall := syscallReportFor(syscallTestCollection(), "")
	lockReport := lockReportFor(lockCollection{samples: []lockSample{
		{Kind: lockKindKernel, Lock: 0xffffffff82000000, LockName: "rtnl_mutex", Flags: lockFlagMutex, Process: "ip", Frames: []string{"__mutex_lock", "rtnl_lock"}, Count: 1, SumNS: 100_000, MaxNS: 100_000},
		{Kind: lockKindFutex, Lock: 0x59a20c, LockName: "main.hotWord", Process: "app", TGID: 42, Frames: []string{"syscall", "worker"}, Count: 5, SumNS: 9_000_000, MaxNS: 2_000_000},
	}})
	priv := privReportFor(privCollection{samples: []privSample{
		{Process: "api", Cap: 21, Checks: 50},
		{Process: "nginx", Cap: 10, Denied: true, Checks: 3},
	}, events: []privEvent{{Event: privEventCap, Process: "nginx", PID: 7, TID: 7, Capability: "CAP_NET_BIND_SERVICE"}}}, false)
	reclaim := reclaimReportFor(reclaimCollection{samples: []reclaimSample{
		{Process: "api", CgroupID: 7, Kind: "memcg", Count: 4, SumNS: 40_000, MaxNS: 15_000, Reclaimed: 256},
		{Process: "db", CgroupID: 8, Kind: "direct", Count: 2, SumNS: 2_000_000, MaxNS: 1_500_000},
	}, summary: reclaimSummary{DurationMS: 1000}}, "", 4096)
	cpu := cpuReportFor([]cpuStack{
		{TGID: 10, Process: "api", User: []string{"hash", "handle", "main"}, Samples: 6},
		{TGID: 10, Process: "api", User: []string{"read", "handle"}, Kernel: []string{"copy_to_user", "sys_read"}, Samples: 3},
	}, cpuSummary{Samples: 100, DurationMS: 2000, FrequencyHz: 49, CPUs: 4, CountsFull: 1})
	offCPU := cpuReportFor([]cpuStack{
		{TGID: 10, Process: "db", User: []string{"pthread_cond_wait", "worker"}, Kernel: []string{"__schedule", "futex_wait", "__x64_sys_futex"}, Samples: 2, OffCPUNS: 300 * uint64(time.Millisecond)},
	}, cpuSummary{OffCPU: true, DurationMS: 2000})
	code1, short := int32(1), 120.0
	execs := newExecAggregator("")
	for _, event := range []execEvent{
		{Event: execEventExec, Process: "curl", Parent: "cron", PID: 10},
		{Event: execEventExit, Process: "curl", Parent: "cron", PID: 10, ExitCode: &code1, LifetimeMS: &short},
		{Event: execEventSignal, Process: "java", PID: 20, Signal: "SIGKILL", Sender: "kworker", FromKernel: true, Result: "delivered"},
	} {
		execs.add(event)
	}
	execReport := execs.report(2*time.Second, execSummary{EventCount: 3})
	ioReport := ioGroupedReportFor(ioFixtureEvents(), ioSummary{EventCount: 2, OmittedBelowThreshold: 196, LostEvents: 1}, traceGroupByDevice)
	return []traceSummaryFixture{
		{"tcp", func() { tcp.print(false) }},
		{"tcp detail", func() { tcp.print(true) }},
		{"udp", func() { udp.print(false) }},
		{"udp detail", func() { udp.print(true) }},
		{"group tcp", func() { printTraceGroupReport(group) }},
		{"group arp", func() { printTraceGroupReport(arpGroup) }},
		{"neighbor", func() { neighbor.print(false) }},
		{"dns", func() { dns.print(false) }},
		{"http", func() { http.print(false) }},
		{"mysql", func() { mysql.print(false) }},
		{"postgres", func() { postgres.print(false) }},
		{"redis", func() { redis.print(false) }},
		{"clickhouse", func() { clickhouse.print(false) }},
		{"socket", func() { socket.print(false) }},
		{"drop", func() { drop.print(false) }},
		{"syscall", func() { syscall.print(os.Stdout) }},
		{"lock", func() { lockReport.print(os.Stdout) }},
		{"priv", func() { priv.print(os.Stdout) }},
		{"reclaim", func() { reclaim.print(os.Stdout) }},
		{"cpu", func() { cpu.print(os.Stdout) }},
		{"off-cpu", func() { offCPU.print(os.Stdout) }},
		{"exec", func() { execReport.print() }},
		{"io", func() { printIOReport(ioReport, ioFixtureEvents(), true) }},
	}
}
