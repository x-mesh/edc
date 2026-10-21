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

type historyNavigable interface {
	navigate(string, *sql.DB, string) (tea.Model, bool, error)
}

type historyInputCapture interface {
	capturesInput() bool
}

func newHistoryNavigator(db *sql.DB, path string, root historyReviewBrowser) historyNavigator {
	root.hosted = true
	root.extraHelp = "history.list_browser.navigator_help"
	root.snapshotHelp = "history.list_browser.snapshot_navigator_help"
	return historyNavigator{db: db, path: path, stack: []tea.Model{root}}
}

func (model historyNavigator) current() tea.Model { return model.stack[len(model.stack)-1] }

func (model historyNavigator) Init() tea.Cmd { return model.current().Init() }

func (model historyNavigator) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		captures := false
		if input, ok := model.current().(historyInputCapture); ok {
			captures = input.capturesInput()
		}
		if key.String() == "ctrl+c" || key.String() == "q" && !captures {
			return model, tea.Quit
		}
		if !captures && key.String() == "esc" && len(model.stack) > 1 && navigatorAtTop(model.current()) {
			model.stack = model.stack[:len(model.stack)-1]
			return model, nil
		}
		if !captures {
			if child, ok := model.current().(historyNavigable); ok {
				next, handled, err := child.navigate(key.String(), model.db, model.path)
				if err != nil {
					if root, ok := model.current().(historyReviewBrowser); ok {
						root.notice, root.noticeSticky = err.Error(), true
						root.resize()
						model.stack[len(model.stack)-1] = root
					} else if sample, ok := model.current().(historySampleBrowser); ok {
						sample.notice = err.Error()
						sample.resize()
						model.stack[len(model.stack)-1] = sample
					}
					return model, nil
				}
				if next != nil && !handled {
					model.stack[len(model.stack)-1] = next
					return model, nil
				}
				if handled {
					if model.size != nil {
						next, _ = next.Update(*model.size)
					}
					if sample, ok := next.(historySampleBrowser); ok {
						sample.hosted = true
						next = sample
					}
					if review, ok := next.(historyReviewBrowser); ok {
						review.hosted = true
						next = review
					}
					model.stack = append(model.stack, next)
					return model, next.Init()
				}
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
	case historyOverviewBrowser:
		return child.atTop()
	}
	return true
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
