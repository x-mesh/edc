package edc

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

const (
	selectGroupLabel     = "group(대상)"
	selectInventoryLabel = "inventory 파일"
	selectRecipeLabel    = "recipe 파일"
	selectHelp           = "을 선택하세요   ↑/↓ 이동   Enter 선택   q 취소"
	selectGroupTitle     = selectGroupLabel + selectHelp
	selectInventoryTitle = selectInventoryLabel + selectHelp
	selectRecipeTitle    = selectRecipeLabel + selectHelp
)

// selectItem은 화면에 보이는 설명과 실제로 고른 값을 나눈다. 경로 선택에서 둘이 다르다.
type selectItem struct {
	label string
	value string
}

func selectItemsFromValues(values []string) []selectItem {
	items := make([]selectItem, 0, len(values))
	for _, value := range values {
		items = append(items, selectItem{label: value, value: value})
	}
	return items
}

type selectModel struct {
	title     string // 고르는 동안 보여 주는 질문과 키 안내
	label     string // 선택을 마친 뒤 남기는 이름표
	items     []selectItem
	cursor    int
	color     bool
	done      bool
	cancelled bool
}

func newSelectModel(title, label string, items []selectItem) selectModel {
	return selectModel{title: title, label: label, items: items, color: true}
}

// withCursorAt은 기본값이 있는 목록에서 그 항목에 커서를 올려 둔다.
func (model selectModel) withCursorAt(value string) selectModel {
	for index, item := range model.items {
		if item.value == value {
			model.cursor = index
			break
		}
	}
	return model
}

func (model selectModel) Init() tea.Cmd { return nil }

func (model selectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, isKey := msg.(tea.KeyPressMsg)
	if !isKey || len(model.items) == 0 {
		return model, nil
	}
	switch key.String() {
	case "up", "k":
		model.cursor = (model.cursor - 1 + len(model.items)) % len(model.items)
	case "down", "j":
		model.cursor = (model.cursor + 1) % len(model.items)
	case "enter":
		model.done = true
		return model, tea.Quit
	case "esc", "q", "ctrl+c":
		model.cancelled = true
		return model, tea.Quit
	}
	return model, nil
}

// View는 목록을 그 자리에 그린다. 줄 수는 처음부터 끝까지 같아야 renderer가 이전 화면을 남기지 않는다.
func (model selectModel) View() tea.View {
	var builder strings.Builder
	builder.WriteString(model.headline() + "\n")
	width := model.rowWidth()
	for index, item := range model.items {
		if index != model.cursor || model.cancelled {
			builder.WriteString(liveIdleBar + " " + item.label + "\n")
			continue
		}
		row := liveSelectedBar + " " + item.label
		// 고르는 동안에는 줄 전체를 반전해 눈에 띄게 한다.
		if !model.done {
			builder.WriteString(liveSelected(liveCell(row, width), model.color) + "\n")
			continue
		}
		// 고른 뒤에는 막대만 남겨 기록으로 조용히 둔다.
		builder.WriteString(row + "\n")
	}
	return liveFrame(builder.String(), model.frameHeight())
}

func (model selectModel) frameHeight() int {
	return 1 + len(model.items)
}

// rowWidth는 반전 칠이 모든 줄에서 같은 폭이 되도록 가장 긴 항목에 맞춘다.
func (model selectModel) rowWidth() int {
	width := 0
	for _, item := range model.items {
		width = max(width, liveWidth(item.label))
	}
	return width + liveWidth(liveSelectedBar) + 1
}

// headline은 첫 줄이다. 고르기 전에는 질문과 키 안내, 고른 뒤에는 이름표만 남긴다.
// 고른 값은 아래 목록의 › 표시가 보여 주므로 여기서 다시 쓰지 않는다.
func (model selectModel) headline() string {
	switch {
	case model.cancelled:
		return model.label + ": 취소"
	case model.done:
		return model.label
	default:
		return model.title
	}
}

func (model selectModel) choice() (string, error) {
	if model.cancelled || !model.done {
		return "", errRemoteCancelled
	}
	return model.items[model.cursor].value, nil
}
