package edc

import (
	"errors"
	"io"
	"os"

	tea "charm.land/bubbletea/v2"
)

func selectRemoteGroup(input *os.File, output io.Writer, groups []string) (string, error) {
	return runSelect(input, output, newSelectModel(selectGroupTitle, selectGroupLabel, selectItemsFromValues(groups)))
}

// runSelect는 선택기를 띄우고 취소를 errRemoteCancelled로 통일한다.
func runSelect(input *os.File, output io.Writer, model selectModel) (string, error) {
	program := tea.NewProgram(model, tea.WithInput(input), tea.WithOutput(output))
	final, err := program.Run()
	if err != nil {
		if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, tea.ErrProgramKilled) {
			return "", errRemoteCancelled
		}
		return "", err
	}
	chosen, ok := final.(selectModel)
	if !ok {
		return "", errRemoteCancelled
	}
	// 고른 값은 실행 머리말이 다시 정확히 보여 주므로 따로 남기지 않는다.
	return chosen.choice()
}

// runConfirm은 확인 위젯을 띄운다. 취소는 errRemoteCancelled다.
func runConfirm(input *os.File, output io.Writer, question string, initial bool) (bool, error) {
	return runConfirmModel(input, output, newConfirmModel(question, initial))
}

func runConfirmModel(input *os.File, output io.Writer, model confirmModel) (bool, error) {
	final, err := tea.NewProgram(model, tea.WithInput(input), tea.WithOutput(output)).Run()
	if err != nil {
		if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, tea.ErrProgramKilled) {
			return false, errRemoteCancelled
		}
		return false, err
	}
	answered, ok := final.(confirmModel)
	if !ok || answered.cancelled {
		return false, errRemoteCancelled
	}
	return answered.yes, nil
}
