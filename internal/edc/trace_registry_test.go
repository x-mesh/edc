package edc

import (
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestTraceProtocolSelectItemsUseRegistryOrder(t *testing.T) {
	items := traceProtocolSelectItems()
	got := make([]string, 0, len(items))
	for _, item := range items {
		if item.label != item.value {
			t.Fatalf("item = %#v", item)
		}
		got = append(got, item.value)
	}
	want := []string{"tcp", "udp", "dns", "arp", "ndp", "http", "mysql", "io", "socket", "drop"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("protocols = %#v, want %#v", got, want)
	}
}

func TestRunTraceSelectsAProtocolOnlyOnTerminals(t *testing.T) {
	previousTerminal := traceIsTerminal
	previousSelector := runTraceProtocolSelector
	previousRegistry := append([]traceProtocolRegistration(nil), traceProtocolRegistry...)
	previousProtocols := traceProtocols
	traceProtocols = traceProtocolSpecs(previousRegistry)
	t.Cleanup(func() {
		traceIsTerminal = previousTerminal
		runTraceProtocolSelector = previousSelector
		traceProtocolRegistry = previousRegistry
		traceProtocols = previousProtocols
	})

	selected := false
	traceIsTerminal = func(*os.File) bool { return true }
	runTraceProtocolSelector = func(_ *os.File, _ io.Writer, items []selectItem) (string, error) {
		selected = true
		if !reflect.DeepEqual(items, traceProtocolSelectItems()) {
			t.Fatalf("items = %#v", items)
		}
		return "test", nil
	}
	registerTraceProtocol(traceProtocolRegistration{name: "test", run: func(args []string) int {
		if len(args) != 0 {
			t.Fatalf("args = %#v", args)
		}
		return 7
	}})
	if code := runTrace(nil); code != 7 || !selected {
		t.Fatalf("code = %d, selected = %t", code, selected)
	}
}

func TestRunTraceKeepsUsageOutsideTerminals(t *testing.T) {
	previousTerminal := traceIsTerminal
	previousSelector := runTraceProtocolSelector
	t.Cleanup(func() {
		traceIsTerminal = previousTerminal
		runTraceProtocolSelector = previousSelector
	})
	traceIsTerminal = func(*os.File) bool { return false }
	runTraceProtocolSelector = func(*os.File, io.Writer, []selectItem) (string, error) {
		t.Fatal("selector called outside a terminal")
		return "", nil
	}
	stderr := captureTraceStderr(t, func() {
		if code := runTrace(nil); code != 2 {
			t.Fatalf("code = %d", code)
		}
	})
	if want := "usage: edc trace <tcp|udp|dns|arp|ndp|http|drop|mysql|io> [options]\nusage: " + traceSocketUsage + "\n"; stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
}

func TestRunTraceKeepsUsageForAnUnknownProtocol(t *testing.T) {
	stderr := captureTraceStderr(t, func() {
		if code := runTrace([]string{"unknown"}); code != 2 {
			t.Fatalf("code = %d", code)
		}
	})
	if !strings.Contains(stderr, "edc trace <tcp|udp|dns|arp|ndp|http|drop|mysql|io> [options]") {
		t.Fatalf("stderr = %q", stderr)
	}
}
