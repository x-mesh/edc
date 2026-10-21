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
		if key.String() == "esc" && len(model.stack) > 1 {
			model.stack = model.stack[:len(model.stack)-1]
			return model, nil
		}
		if root, ok := model.current().(historyReviewBrowser); ok && !root.expanded && len(root.data.entries) > 0 {
			options := historyOptions{run: root.data.entries[root.selected].id}
			switch key.String() {
			case "t":
				samples, err := loadHistoryTop(model.db, options)
				if err != nil {
					return model, nil
				}
				child := newHistoryTopBrowser(samples, model.path)
				child.load = func() ([]historyTopSample, error) { return loadHistoryTop(model.db, options) }
				model.stack = append(model.stack, child)
				return model, child.Init()
			case "p":
				samples, err := loadHistoryProcess(model.db, options)
				if err != nil {
					return model, nil
				}
				child := newHistoryProcessBrowser(samples, model.path)
				child.loadProcesses = func() ([]historyProcessSample, error) { return loadHistoryProcess(model.db, options) }
				model.stack = append(model.stack, child)
				return model, child.Init()
			}
		}
	}
	updated, command := model.current().Update(msg)
	model.stack[len(model.stack)-1] = updated
	return model, command
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
