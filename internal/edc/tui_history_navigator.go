package edc

import (
	"database/sql"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
)

type historyNavigator struct {
	db    *sql.DB
	path  string
	stack []tea.Model
	size  *tea.WindowSizeMsg
}

func newHistoryNavigator(db *sql.DB, path string, root historyReviewBrowser) historyNavigator {
	return historyNavigator{db: db, path: path, stack: []tea.Model{root}}
}

func (model historyNavigator) current() tea.Model { return model.stack[len(model.stack)-1] }

func (model historyNavigator) Init() tea.Cmd { return model.current().Init() }

func (model historyNavigator) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		if key.String() == "q" || key.String() == "ctrl+c" {
			return model, tea.Quit
		}
		if key.String() == "esc" && len(model.stack) > 1 && navigatorAtTop(model.current()) {
			model.stack = model.stack[:len(model.stack)-1]
			return model, nil
		}
		if root, ok := model.current().(historyReviewBrowser); ok && !root.expanded && len(root.data.entries) > 0 {
			options := historyOptions{run: root.data.entries[root.selected].id}
			switch key.String() {
			case "t":
				if historyNavigatorSnapshot(root) {
					root.notice = T("history.list_browser.snapshot_navigation")
					root.noticeSticky = true
					root.resize()
					model.stack[len(model.stack)-1] = root
					return model, nil
				}
				samples, err := loadHistoryTop(model.db, options)
				if err != nil {
					root.notice = err.Error()
					root.resize()
					model.stack[len(model.stack)-1] = root
					return model, nil
				}
				child := newHistoryTopBrowser(samples, model.path)
				child.load = func() ([]historyTopSample, error) { return loadHistoryTop(model.db, options) }
				if model.size != nil {
					updated, _ := child.Update(*model.size)
					child = updated.(historySampleBrowser)
				}
				model.stack = append(model.stack, child)
				return model, child.Init()
			case "o":
				if historyNavigatorSnapshot(root) {
					root.notice = T("history.list_browser.snapshot_navigation")
					root.noticeSticky = true
					root.resize()
					model.stack[len(model.stack)-1] = root
					return model, nil
				}
				samples, err := loadHistoryProcess(model.db, options)
				if err != nil {
					root.notice = err.Error()
					root.resize()
					model.stack[len(model.stack)-1] = root
					return model, nil
				}
				child := newHistoryProcessBrowser(samples, model.path)
				child.loadProcesses = func() ([]historyProcessSample, error) { return loadHistoryProcess(model.db, options) }
				if model.size != nil {
					updated, _ := child.Update(*model.size)
					child = updated.(historySampleBrowser)
				}
				model.stack = append(model.stack, child)
				return model, child.Init()
			}
		}
		updated, command := model.current().Update(msg)
		model.stack[len(model.stack)-1] = updated
		return model, command
	}
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		model.size = &size
	}
	var commands []tea.Cmd
	for index, child := range model.stack {
		updated, command := child.Update(msg)
		model.stack[index] = updated
		if command != nil {
			commands = append(commands, command)
		}
	}
	return model, tea.Batch(commands...)
}

func navigatorAtTop(model tea.Model) bool {
	switch child := model.(type) {
	case historyReviewBrowser:
		return child.atTop()
	case historySampleBrowser:
		return child.atTop()
	}
	return true
}

func historyNavigatorSnapshot(root historyReviewBrowser) bool {
	return root.data.entries[root.selected].command == historySnapshotCommand
}

func (model historyNavigator) View() tea.View { return model.current().View() }

func runHistoryNavigator() error {
	path, err := defaultHistoryPath()
	if err != nil {
		return err
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		return err
	}
	defer db.Close()
	data, err := historyListReviewData(db, historyOptions{}, time.Now())
	if err != nil {
		return err
	}
	root := newHistoryListBrowser(path, data)
	root.load = func() (historyReviewData, error) { return historyListReviewData(db, historyOptions{}, time.Now()) }
	_, err = tea.NewProgram(newHistoryNavigator(db, path, root), tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run()
	return err
}
