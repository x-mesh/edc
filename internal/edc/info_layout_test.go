package edc

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestInfoLayoutPreservesValuesAtDifferentWidths(t *testing.T) {
	for _, width := range []int{40, 60, 80, 140} {
		for _, color := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/%t", width, color), func(t *testing.T) {
				style := infoStyle{textStyle{color: color, width: width}}
				var output strings.Builder
				style.tree(&output, "├──", style.join(" · ", style.kv("Used", "30.58 GB / 48.00 GB (63.72%)"), style.kv("Available", "17.42 GB"), style.kv("Swap", "none")))
				style.tree(&output, "└──", style.warn("read-only · inodes 95% · deleted-open 12.0 GB in 3 files"))
				text := ansi.Strip(output.String())
				for _, row := range strings.Split(text, "\n") {
					if liveWidth(row) > width {
						t.Fatalf("line exceeds width: %q", row)
					}
				}
				for _, want := range []string{"30.58 GB", "48.00 GB", "63.72%", "17.42 GB", "Swap: none", "read-only", "95%", "12.0 GB"} {
					if !strings.Contains(text, want) {
						t.Fatalf("lost %q: %s", want, text)
					}
				}
				if width == 140 && strings.Count(text, "\n") != 2 {
					t.Fatalf("wide output wraps unnecessarily: %s", text)
				}
				if width == 40 && strings.Count(text, "\n") <= 2 {
					t.Fatalf("narrow output should wrap: %s", text)
				}
			})
		}
	}
}

func TestInfoDiskLayoutAdaptsWithoutLosingWarnings(t *testing.T) {
	mount := t.TempDir()
	disks := []diskDetails{{Mount: "/", Device: "/dev/root", Used: 783 << 30, Total: 926 << 30, Percent: 84.58}, {Mount: mount, Device: "/Applications/Tapo.app/Wrapper", Used: 808 << 30, Total: 926 << 30, Percent: 87.22}}
	for _, width := range []int{40, 80, 140} {
		for _, color := range []bool{false, true} {
			for _, verbose := range []bool{false, true} {
				style := infoStyle{textStyle{color: color, width: width}}
				var output strings.Builder
				printDiskUsageStyled(&output, disks, map[string]diskSignal{mount: {ReadOnly: true, InodesUsed: 95, InodesTotal: 100}}, style, verbose)
				text := ansi.Strip(output.String())
				for _, row := range strings.Split(text, "\n") {
					if liveWidth(row) > width {
						t.Fatalf("width %d: %q", width, row)
					}
				}
				for _, want := range []string{"783.00 GB", "926.00 GB", "143.00 GB", "84.58%", "87.22%", "read-only", "inodes 95%"} {
					if !strings.Contains(text, want) {
						t.Fatalf("width %d lost %q: %s", width, want, text)
					}
				}
				root := strings.Split(text, "\n")[2]
				if width == 140 && !strings.Contains(root, "84.58%") {
					t.Fatalf("wide root should fit: %s", text)
				}
				if width == 40 && strings.Contains(root, "84.58%") {
					t.Fatalf("narrow root should split: %s", text)
				}
				if verbose {
					compact := strings.ReplaceAll(strings.ReplaceAll(text, "\n", ""), "    ", "")
					if !strings.Contains(compact, mount) {
						t.Fatalf("verbose lost full mount: %s", text)
					}
				}
			}
		}
	}
}

func TestInfoInterfaceCIDR(t *testing.T) {
	for _, mask := range []struct{ mask, want string }{{"255.255.255.0", "192.0.2.1/24"}, {"255.255.255.255", "192.0.2.1/32"}, {"255.0.255.0", "192.0.2.1 / 255.0.255.0"}} {
		if got := infoInterfaceCIDR(interfaceDetails{Address: "192.0.2.1", Mask: mask.mask}); got != mask.want {
			t.Fatalf("%s: %s", mask.mask, got)
		}
	}
}
