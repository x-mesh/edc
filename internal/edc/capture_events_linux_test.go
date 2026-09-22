//go:build linux

package edc

import "testing"

func TestCaptureEventAddressFormatting(t *testing.T) {
	var address [16]byte
	address[0], address[1], address[2], address[3] = 192, 0, 2, 10
	if got := formatCaptureAddress(2, address, 443); got != "192.0.2.10:443" {
		t.Fatalf("IPv4 address = %q", got)
	}
	address[15] = 1
	if got := formatCaptureAddress(10, address, 443); got != "[c000:20a:0:0:0:0:0:1]:443" {
		t.Fatalf("IPv6 address = %q", got)
	}
}

func TestTraceTargetFromArguments(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"curl", "https://naver.com/path"}, "naver.com"},
		{[]string{"curl", "naver.com"}, "naver.com"},
		{[]string{"curl", "http://127.0.0.1:8080/health"}, "127.0.0.1"},
		{[]string{"curl", "--fail", "https://example.com"}, "example.com"},
	}
	for _, test := range cases {
		if got := traceTargetFromArguments(test.args); got != test.want {
			t.Fatalf("traceTargetFromArguments(%q) = %q, want %q", test.args, got, test.want)
		}
	}
}
