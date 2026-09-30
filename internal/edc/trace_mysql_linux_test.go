//go:build linux

package edc

import (
	"encoding/binary"
	"strings"
	"testing"
)

func TestParseMySQLRecordReadsOnlyMySQLRecords(t *testing.T) {
	sample := make([]byte, httpRecordPayloadOffset+8)
	binary.LittleEndian.PutUint64(sample[0:8], 5_000)
	binary.LittleEndian.PutUint32(sample[8:12], mysqlRecordType)
	binary.LittleEndian.PutUint32(sample[12:16], 42)
	binary.LittleEndian.PutUint64(sample[24:32], 0xabc)
	// len은 payload에 담은 byte 수이고 offset은 그 읽기의 전체 byte 수다.
	binary.LittleEndian.PutUint32(sample[32:36], 5)
	binary.LittleEndian.PutUint16(sample[36:38], 2)
	sample[38], sample[39] = httpRecordSent, mysqlRecordServer
	binary.LittleEndian.PutUint16(sample[40:42], 3306)
	binary.LittleEndian.PutUint16(sample[42:44], 40000)
	copy(sample[44:48], []byte{10, 0, 0, 2})
	copy(sample[60:64], []byte{192, 0, 2, 1})
	copy(sample[76:], "mysqld")
	binary.LittleEndian.PutUint32(sample[92:96], 9000)
	copy(sample[httpRecordPayloadOffset:], "abcdefgh")
	record, ok := parseMySQLRecord(sample)
	if !ok || !record.sent || !record.server || record.size != 9000 || string(record.payload) != "abcde" || record.socket != 0xabc || record.pid != 42 || record.bootTimeNS != 5_000 ||
		record.process != "mysqld" || record.source != "10.0.0.2:3306" || record.destination != "192.0.2.1:40000" {
		t.Fatalf("parseMySQLRecord = %#v, %t", record, ok)
	}
	sample[38], sample[39] = 0, 0
	if record, ok := parseMySQLRecord(sample); !ok || record.sent || record.server {
		t.Fatalf("client receive = %#v, %t", record, ok)
	}
	// HTTP 레코드는 MySQL이 아니고, MySQL 레코드는 HTTP parser가 받지 않는다.
	binary.LittleEndian.PutUint32(sample[8:12], httpRecordType)
	if _, ok := parseMySQLRecord(sample); ok {
		t.Fatal("an HTTP record was read as MySQL")
	}
	binary.LittleEndian.PutUint32(sample[8:12], mysqlRecordType)
	if _, ok := parseHTTPRecord(sample); ok {
		t.Fatal("a MySQL record was read as HTTP")
	}
	if _, ok := parseMySQLRecord(sample[:httpRecordPayloadOffset-1]); ok {
		t.Fatal("a short record was accepted")
	}
}

func TestMySQLTraceOptionChecks(t *testing.T) {
	for _, test := range []struct {
		args   []string
		stderr string
	}{
		// --show-secrets는 --payload 없이 받는다. 그래서 다음 검사인 port 범위에서 멈춘다.
		{[]string{"mysql", "--show-secrets", "--port", "0"}, "--port must be from 1 to 65535"},
		{[]string{"mysql", "--payload"}, "--payload is not available for trace mysql"},
		{[]string{"tcp", "--show-secrets"}, "--show-secrets needs --payload"},
	} {
		var code int
		stderr := captureTraceStderr(t, func() { code = runTrace(test.args) })
		if code != 2 || !strings.Contains(stderr, test.stderr) {
			t.Fatalf("trace %q exit = %d, stderr %q, want 2 and %q", test.args, code, stderr, test.stderr)
		}
	}
}

func TestMySQLTraceSlowOptionChecks(t *testing.T) {
	for _, test := range []struct {
		args   []string
		stderr string
	}{
		{[]string{"mysql", "--slow", "250us", "--port", "0"}, "--port must be from 1 to 65535"},
		{[]string{"mysql", "--slow", "1.5ms", "--port", "0"}, "--port must be from 1 to 65535"},
		{[]string{"mysql", "--slow", "0s"}, "--slow must be greater than 0"},
		{[]string{"mysql", "--slow", "-1ms"}, "--slow must be greater than 0"},
		{[]string{"mysql", "--slow", "slow"}, "invalid value \"slow\" for flag -slow"},
		{[]string{"mysql", "--slow", "1000000000000000000000h"}, "invalid value \"1000000000000000000000h\" for flag -slow"},
		{[]string{"tcp", "--slow", "1ms"}, "--slow is only available for trace mysql"},
		{[]string{"http", "--slow", "1ms"}, "--slow is only available for trace mysql"},
	} {
		var code int
		stderr := captureTraceStderr(t, func() { code = runTrace(test.args) })
		if code != 2 || !strings.Contains(stderr, test.stderr) {
			t.Fatalf("trace %q exit = %d, stderr %q, want 2 and %q", test.args, code, stderr, test.stderr)
		}
	}
}
