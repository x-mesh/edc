package edc

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func topSetupFixture(width, height int, top topConfig) topSetupModel {
	preview := topSplitModelAt(width, height, 30, false, nil)
	preview.view, preview.split = topViewAll, nil
	health := &networkHealthRate{networkHealth: networkHealth{Supported: true}}
	for index := range preview.rows {
		preview.rows[index].rate.NetworkHealth = health
	}
	source := &fakeTopAISource{result: topAIFixtureResult()}
	preview.openAI = func() (topAISource, error) { return source, nil }
	config := edcConfig{Defaults: configDefaults{Top: top, Info: infoConfig{Public: boolPointer(true)}}}
	model := newTopSetupModel(preview, config, "/tmp/edc/config.toml")
	model.width, model.height = width, height
	return model
}

func topSetupKeys(t *testing.T, model topSetupModel, keys ...string) topSetupModel {
	t.Helper()
	for _, key := range keys {
		updated, _ := model.updateKey(key)
		model = updated.(topSetupModel)
	}
	return model
}

func TestTopSetupKeepsUntouchedValues(t *testing.T) {
	for _, top := range []topConfig{
		{},
		{Split: stringPointer("net,cpu"), Panels: stringPointer("ai"), View: stringPointer("network"), ProcessSort: stringPointer("io"),
			Interval: &configDuration{Duration: 2 * time.Second}, Count: intPointer(3)},
		{Split: stringPointer("all"), Panels: stringPointer("none")},
		{Split: stringPointer("cpu,mem,disk,net,psi"), Panels: stringPointer(" net"), View: stringPointer(""), Interval: &configDuration{Duration: time.Second}},
		{Columns: stringPointer("net+, cpu")},
		{Columns: stringPointer("net,cpu,mem,disk,psi")},
	} {
		model := topSetupFixture(120, 40, top)
		if model.dirty() || !reflect.DeepEqual(model.settings.topConfig(top), top) {
			t.Fatalf("an untouched setup changed %#v into %#v", top, model.settings.topConfig(top))
		}
	}
	// 보기만 바꾸면 박스와 패널은 파일에 적지 않는다. 기본 배치가 바뀌어도 따라간다.
	model := topSetupKeys(t, topSetupFixture(120, 40, topConfig{}), "right")
	top := model.settings.topConfig(topConfig{})
	if top.View == nil || *top.View != "all" || top.Split != nil || top.Panels != nil || !model.dirty() {
		t.Fatalf("view change wrote %#v", top)
	}
	if text := ansi.Strip(model.View().Content); !strings.Contains(text, "view*") || strings.Contains(text, "boxes*") {
		t.Fatalf("only the changed row gets a mark:\n%s", text)
	}
}

func TestTopSetupBoxesFollowTheList(t *testing.T) {
	model := topSetupKeys(t, topSetupFixture(160, 50, topConfig{}), "down", "down", "right", "right", "right", "[", "[", "[")
	if got := model.settings.splitValue(); got != "net,cpu,mem,disk,psi" || model.item != 0 {
		t.Fatalf("split %q, item %d", got, model.item)
	}
	model = topSetupKeys(t, model, "right", "space", "right", "space", "right", "right", "space")
	if got := model.settings.splitValue(); got != "net,disk" {
		t.Fatalf("split %q, want net,disk", got)
	}
	if model.preview.view != topViewSplit || !reflect.DeepEqual(model.preview.splitViews(), []topView{topViewNetwork, topViewDisk}) {
		t.Fatalf("preview view %q, boxes %v", model.preview.view, model.preview.splitViews())
	}
	text := ansi.Strip(model.View().Content)
	if !strings.Contains(text, "╭─ net") || strings.Index(text, "╭─ net") > strings.Index(text, "╭─ disk") {
		t.Fatalf("preview must draw net before disk:\n%s", text)
	}
	all := topSetupKeys(t, topSetupFixture(120, 40, topConfig{}), "down", "down", "space", "space")
	if got := all.settings.splitValue(); got != "all" {
		t.Fatalf("five boxes in the default order = %q, want all", got)
	}
	none := topSetupKeys(t, all, "space", "right", "space", "right", "space", "right", "space", "right", "space")
	if got := none.settings.splitValue(); got != "none" {
		t.Fatalf("no boxes = %q, want none", got)
	}
}

func TestTopSetupPanelsShowInThePreviewAndNoteWhatHides(t *testing.T) {
	model := topSetupKeys(t, topSetupFixture(topTableWidth, 24+topSetupChrome, topConfig{}), "down", "down", "down", "right", "right", "right", "space", "right", "right", "space")
	if got := model.settings.panelsValue(); got != "net,ai" {
		t.Fatalf("panels %q", got)
	}
	if !model.preview.aiOn || model.preview.aiSource == nil {
		t.Fatal("ai in the list must start the AI panel in the preview")
	}
	if !strings.Contains(ansi.Strip(model.View().Content), "80×24 · ai polls Claude, Codex") {
		t.Fatal("turning on ai must say that the preview reads the usage")
	}
	model = topSetupKeys(t, model, "[", "[")
	text := ansi.Strip(model.View().Content)
	// 80×24 미리보기에서는 process 후보 자리를 남기므로 10줄인 net 상세가 들어가지 않는다.
	if !strings.Contains(text, "80×24 · ai polls Claude, Codex · +net hidden") {
		t.Fatalf("setup must say which panel hides at this size:\n%s", text)
	}
	if model.settings.panelsValue() != "ai,net" {
		t.Fatalf("[ moved the panel to %q", model.settings.panelsValue())
	}
}

func TestTopSetupSizesSkipWhatDoesNotFit(t *testing.T) {
	model := topSetupFixture(100, 30, topConfig{})
	if model = topSetupKeys(t, model, "z"); model.size != 0 {
		t.Fatalf("80×24 does not fit in 100×%d, size = %d", 30-topSetupChrome, model.size)
	}
	model = topSetupKeys(t, model, "tab")
	if model = topSetupKeys(t, model, "z"); model.size != 1 {
		t.Fatalf("full screen 100×30 fits 80×24, size = %d", model.size)
	}
	if model = topSetupKeys(t, model, "z"); model.size != 0 {
		t.Fatalf("120×30 does not fit in 100×30, size = %d", model.size)
	}
	// 전체 화면은 저장 후의 edc top과 같은 크기로 그린다.
	lines := strings.Split(model.View().Content, "\n")
	if len(lines) != 30 || !strings.Contains(ansi.Strip(lines[0]), "view all") || !strings.Contains(ansi.Strip(model.View().Content), "setup preview · Tab back") {
		t.Fatalf("full preview has %d lines:\n%s", len(lines), ansi.Strip(model.View().Content))
	}
	for _, line := range strings.Split(topSetupFixture(100, 30, topConfig{}).View().Content, "\n") {
		if width := ansi.StringWidth(line); width > 100 {
			t.Fatalf("line is %d columns: %q", width, ansi.Strip(line))
		}
	}
}

func TestTopSetupSavesOnlyTheTopSection(t *testing.T) {
	model := topSetupKeys(t, topSetupFixture(120, 40, topConfig{Count: intPointer(3)}), "down", "down", "down", "space", "enter")
	if !model.confirmSave || !strings.Contains(ansi.Strip(model.View().Content), "panels = 'cpu'") {
		t.Fatalf("Enter must show the section to save:\n%s", ansi.Strip(model.View().Content))
	}
	updated, cmd := model.updateKey("y")
	model = updated.(topSetupModel)
	if !model.save || cmd == nil {
		t.Fatal("y must save and quit")
	}
	data, err := model.configData()
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"count = 3", "panels = 'cpu'", "[defaults.info]", "public = true"} {
		if !strings.Contains(text, want) {
			t.Fatalf("saved config lacks %q:\n%s", want, text)
		}
	}
}

func TestTopSetupAsksBeforeDiscardingChanges(t *testing.T) {
	if _, cmd := topSetupFixture(120, 40, topConfig{}).updateKey("esc"); cmd == nil {
		t.Fatal("Esc without changes must quit")
	}
	model := topSetupKeys(t, topSetupFixture(120, 40, topConfig{}), "right", "esc")
	if !model.confirmQuit {
		t.Fatal("Esc with changes must ask first")
	}
	if model = topSetupKeys(t, model, "n"); model.confirmQuit || !model.dirty() {
		t.Fatal("n must keep the changes")
	}
	model = topSetupKeys(t, model, "esc")
	updated, cmd := model.updateKey("y")
	if updated.(topSetupModel).save || cmd == nil {
		t.Fatal("y must quit without saving")
	}
}

func TestTopSetupNeedsATerminal(t *testing.T) {
	for _, args := range [][]string{{"setup"}, {"setup", "extra"}} {
		if code := runTop(args, "test"); code != 2 {
			t.Errorf("runTop(%v) = %d, want 2", args, code)
		}
	}
}

func TestTopSetupColumnsChooseGroupsAndDetail(t *testing.T) {
	model := topSetupKeys(t, topSetupFixture(120, 40, topConfig{}), "down", "+", "right", "right", "right", "space", "right", "space")
	if got := model.settings.columnsValue(); got != "net+,cpu,mem" {
		t.Fatalf("columns %q, want net+,cpu,mem", got)
	}
	if !reflect.DeepEqual(model.preview.columnGroups, []topColumnGroup{{"network", true}, {"cpu", false}, {"mem", false}}) {
		t.Fatalf("preview groups %v", model.preview.columnGroups)
	}
	text := ansi.Strip(model.View().Content)
	if !strings.Contains(text, "[x] net+") || strings.Contains(text, "--- disk") {
		t.Fatalf("columns row or preview header is wrong:\n%s", text)
	}
	// 마지막 그룹은 끌 수 없다.
	only := topSetupKeys(t, model, "left", "left", "space", "left", "space", "left", "space")
	if got := only.settings.columnsValue(); got != "net+" {
		t.Fatalf("columns %q, want net+ to stay", got)
	}
	split := topSetupKeys(t, topSetupFixture(120, 40, topConfig{View: stringPointer("split")}), "down")
	if !strings.Contains(ansi.Strip(split.View().Content), "all view only · starts in boxes") {
		t.Fatal("columns must say they apply to the all view")
	}
}

func TestTopSetupPreviewFollowsTheRowBeingEdited(t *testing.T) {
	// 박스를 고치면 시작 보기가 박스 화면이 된다. 그 뒤에도 columns 줄에서는 all 보기를, boxes 줄에서는 박스를 그린다.
	model := topSetupKeys(t, topSetupFixture(160, 50, topConfig{}), "down", "down", "space", "up")
	if model.preview.view != topViewSplit {
		t.Fatalf("start view %q, want boxes after a box change", model.preview.view)
	}
	text := ansi.Strip(model.View().Content)
	if model.sizedPreview().view != topViewAll || !strings.Contains(text, "preview · all view for columns · start view boxes") || strings.Contains(text, "╭─") {
		t.Fatalf("columns row must draw the all view:\n%s", text)
	}
	model = topSetupKeys(t, model, "+")
	if !strings.Contains(ansi.Strip(model.View().Content), "pk_in") {
		t.Fatal("a columns change must show in the preview")
	}
	model = topSetupKeys(t, model, "down")
	if text := ansi.Strip(model.View().Content); model.sizedPreview().view != topViewSplit || !strings.Contains(text, "preview · start view boxes") || !strings.Contains(text, "╭─ mem") {
		t.Fatalf("boxes row must draw the boxes:\n%s", text)
	}
	if full := topSetupKeys(t, model, "up", "tab"); full.sizedPreview().view != topViewSplit {
		t.Fatalf("full preview must draw the start view, got %q", full.sizedPreview().view)
	}
	start := topSetupFixture(160, 50, topConfig{})
	if text := ansi.Strip(topSetupKeys(t, start, "down", "down").View().Content); !strings.Contains(text, "preview · boxes for boxes · start view all") {
		t.Fatalf("boxes row on an all start view must say why it draws boxes:\n%s", text)
	}
}

func TestTopSetupAIColumnsAreOptIn(t *testing.T) {
	model := topSetupFixture(160, 50, topConfig{})
	if model.preview.aiTokens || model.settings.columnsValue() != "all" {
		t.Fatalf("ai columns must start off: %q", model.settings.columnsValue())
	}
	model = topSetupKeys(t, model, "down", "right", "right", "right", "right", "right", "space")
	if got := model.settings.columnsValue(); got != "net,cpu,mem,disk,psi,ai" || !model.preview.aiTokens {
		t.Fatalf("columns %q, tokens %v", got, model.preview.aiTokens)
	}
	if text := ansi.Strip(model.View().Content); !strings.Contains(text, "ai reads Claude, Codex logs") || !strings.Contains(text, " claude ") {
		t.Fatalf("turning on ai columns must say so and draw them:\n%s", text)
	}
}
