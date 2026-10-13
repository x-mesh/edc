package edc

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

var topPickerList = []topInterfaceInfo{{name: "docker0", up: true}, {name: "eno1", up: true}, {name: "veth48baa04"}, {name: "eno2", up: true}}

func topPickerPhysical(name string) bool { return strings.HasPrefix(name, "eno") }

func topPickerModel(t *testing.T, current string, list []topInterfaceInfo) (topModel, *[]string) {
	t.Helper()
	model := topFixtureModel(nil)
	model.limits.color = false
	model.interfaces, _ = parseTopInterfaceFilter(current)
	model.listInterfaces = func() ([]topInterfaceInfo, error) { return list, nil }
	model.interfacePhysical, model.physicalSupported = topPickerPhysical, true
	applied := &[]string{}
	model.setInterfaces = func(filter topInterfaceFilter) { *applied = append(*applied, filter.String()) }
	return model, applied
}

func TestTopInterfacePickerStartsFromTheCurrentSelection(t *testing.T) {
	rates := []netInterfaceRate{{Name: "eno2", In: 1 << 20}}
	current, _ := parseTopInterfaceFilter("physical")
	picker := newTopInterfacePicker(current, topPickerList, rates, topPickerPhysical, true)
	var order []string
	for _, row := range picker.rows {
		order = append(order, row.name)
	}
	// 실제 회선이 먼저이고, 그 안에서는 traffic이 많은 순이다.
	if got := strings.Join(order, ","); got != "eno2,eno1,docker0,veth48baa04" {
		t.Fatalf("rows = %s", got)
	}
	if len(picker.presets) != 2 || picker.preset != 1 || picker.cursor != 1 || !picker.checked["eno1"] || !picker.checked["eno2"] || picker.checked["docker0"] {
		t.Fatalf("physical picker = %+v", picker)
	}
	for _, tc := range []struct {
		current string
		presets int
		preset  int
	}{{"", 2, 0}, {"en*", 3, 2}, {"physical,wg0", 3, 2}, {"eno1,docker0", 2, -1}} {
		filter, _ := parseTopInterfaceFilter(tc.current)
		picker := newTopInterfacePicker(filter, topPickerList, nil, topPickerPhysical, true)
		if len(picker.presets) != tc.presets || picker.preset != tc.preset {
			t.Errorf("%q: presets %d preset %d", tc.current, len(picker.presets), picker.preset)
		}
	}
	if picker := newTopInterfacePicker(topInterfaceFilter{}, topPickerList, nil, topPickerPhysical, false); len(picker.presets) != 1 {
		t.Fatalf("macOS has no physical preset: %+v", picker.presets)
	}
	explicit, _ := parseTopInterfaceFilter("eno1,docker0")
	if picker := newTopInterfacePicker(explicit, topPickerList, nil, topPickerPhysical, true); picker.cursor != len(picker.presets) {
		t.Fatalf("an explicit list starts on its first checked row: cursor %d", picker.cursor)
	}
}

func TestTopInterfacePickerTogglesAndApplies(t *testing.T) {
	model, applied := topPickerModel(t, "physical", topPickerList)
	model = topAfter(t, model, topKey("i"))
	if model.picker == nil || !strings.Contains(model.View().Content, "(•) physical     eno1 eno2") {
		t.Fatalf("i opens the picker:\n%s", model.View().Content)
	}
	// physical에서 docker0 하나를 더하면 이름 목록이 된다.
	model = topAfter(t, model, topKey("down"), topKey("down"), topKey("down"), topKey(" "), topKey("enter"))
	if model.picker != nil || model.interfaces.String() != "eno1,eno2,docker0" || !model.baseline || !reflect.DeepEqual(*applied, []string{"eno1,eno2,docker0"}) {
		t.Fatalf("selection %q, baseline %v, applied %v", model.interfaces, model.baseline, *applied)
	}
	model = topAfter(t, model, topKey("i"), topKey("a"), topKey("enter"))
	if model.interfaces.active() {
		t.Fatalf("a selects every interface: %q", model.interfaces)
	}
	model = topAfter(t, model, topKey("i"), topKey("p"), topKey("enter"))
	if model.interfaces.String() != "physical" {
		t.Fatalf("p selects physical: %q", model.interfaces)
	}
	// 아무것도 체크하지 않으면 상자를 닫지 않고 안내한다.
	model = topAfter(t, model, topKey("i"), topKey("down"), topKey(" "), topKey("down"), topKey(" "), topKey("enter"))
	if model.picker == nil || !strings.Contains(model.View().Content, "check at least one interface") {
		t.Fatalf("an empty list must stay open:\n%s", model.View().Content)
	}
	model = topAfter(t, model, topKey("esc"))
	if model.picker != nil || model.interfaces.String() != "physical" || len(*applied) != 3 {
		t.Fatalf("esc must close without a change: %q, applied %v", model.interfaces, *applied)
	}
	model.record = func(historyTopSample) error { return nil }
	model = topAfter(t, model, topKey("i"))
	if model.picker != nil || !strings.HasPrefix(model.statusLines()[0], T("observe.top.interface_recording")) {
		t.Fatalf("i while recording: picker %v, status %q", model.picker != nil, model.statusLines()[0])
	}
}

func TestTopInterfacePickerKeepsAGlobFromTheCommandLine(t *testing.T) {
	model, applied := topPickerModel(t, "en*", topPickerList)
	model = topAfter(t, model, topKey("i"))
	if !strings.Contains(model.View().Content, "(•) en*          eno1 eno2") {
		t.Fatalf("the glob preset:\n%s", model.View().Content)
	}
	model = topAfter(t, model, topKey("enter"))
	if model.interfaces.String() != "en*" || len(*applied) != 0 || model.baseline {
		t.Fatalf("applying the same glob must not change anything: %q, applied %v", model.interfaces, *applied)
	}
}

func TestTopInterfacePickerShowsTheTrafficOfUnselectedInterfaces(t *testing.T) {
	model, _ := topPickerModel(t, "eno1", topPickerList)
	before := resourceSnapshot{TakenAt: time.Unix(1, 0), NetInterfacesAll: []netInterfaceCounters{{Name: "eno1"}, {Name: "docker0"}}}
	after := resourceSnapshot{TakenAt: time.Unix(2, 0), NetInterfacesAll: []netInterfaceCounters{{Name: "eno1", BytesIn: 1 << 20}, {Name: "docker0", BytesIn: 3 << 20}}}
	model.previous = before
	model = topAfter(t, model, topSampleMsg{seq: model.seq, snapshot: after}, topKey("i"))
	if content := model.View().Content; !strings.Contains(content, "[ ] docker0         virt  in 3.00M") {
		t.Fatalf("docker0 is not selected but its traffic shows:\n%s", content)
	}
}

func TestTopInterfacePickerScrollsAndFitsTheScreen(t *testing.T) {
	var list []topInterfaceInfo
	for index := range 30 {
		list = append(list, topInterfaceInfo{name: fmt.Sprintf("veth%02d", index), up: true})
	}
	model, _ := topPickerModel(t, "", list)
	model = topAfter(t, model, tea.WindowSizeMsg{Width: 80, Height: 24}, topKey("i"))
	content := model.View().Content
	lines := strings.Split(content, "\n")
	if len(lines) > 24 || !strings.Contains(content, "↓ ") || strings.Contains(content, "↑ ") {
		t.Fatalf("first page:\n%s", content)
	}
	for range 31 {
		model = topAfter(t, model, topKey("down"))
	}
	content = model.View().Content
	if !strings.Contains(content, "› [x] veth29") && !strings.Contains(content, "›[x] veth29") || !strings.Contains(content, "↑ ") {
		t.Fatalf("last row after scrolling:\n%s", content)
	}
	for _, width := range []int{80, 40} {
		model = topAfter(t, model, tea.WindowSizeMsg{Width: width, Height: 24})
		for _, line := range strings.Split(model.View().Content, "\n") {
			if got := ansi.StringWidth(line); got > width {
				t.Fatalf("width %d: line is %d wide: %q", width, got, line)
			}
		}
	}
}
