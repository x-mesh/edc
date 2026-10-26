package edc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
)

// viewFollowDefaultLines는 -f에 -n이 없을 때 처음 보여 주는 줄 수다. tail -f와 같다.
const viewFollowDefaultLines = 10

const viewUsage = "edc view [-n N] [-f] [file]"

func runView(args []string) int {
	set := newFlagSet("view")
	set.SetOutput(os.Stderr)
	lines := set.Int("n", -1, T("command.view.option.lines"))
	follow := set.Bool("f", false, T("command.view.option.follow"))
	set.BoolVar(follow, "F", false, T("command.view.option.follow"))
	if err := parseReordered(set, args); err != nil {
		return 2
	}
	if set.NArg() > 1 || *lines < -1 {
		fmt.Fprintln(os.Stderr, T("cli.usage", viewUsage))
		return 2
	}
	path := ""
	if set.NArg() == 1 && set.Arg(0) != "-" {
		path = set.Arg(0)
	}
	if path == "" && isTerminal(os.Stdin) {
		fmt.Fprintln(os.Stderr, T("cli.usage", viewUsage))
		return 2
	}
	// 화면은 사람이 보는 터미널에서만 연다. pipe와 NO_COLOR에서는 cat, tail -F처럼 받은 바이트를 그대로 낸다.
	// stdin이 pipe여도 키는 bubbletea가 터미널을 직접 열어 읽는다.
	if isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == "" {
		return runViewScreen(path, *lines)
	}
	if err := viewPassthrough(os.Stdout, os.Stderr, path, *lines, *follow); err != nil {
		fmt.Fprintln(os.Stderr, T("view.failed", err))
		return 1
	}
	return 0
}

// viewPassthrough는 화면 없이 원문을 낸다. lines가 -1이면 전부, 아니면 마지막 lines줄이다. follow면 파일 끝에서 기다리며
// 새로 쓰인 바이트를 낸다. 파일이 바뀐 것은 stdout 원문에 섞지 않고 stderr에 알린다.
func viewPassthrough(stdout, stderr io.Writer, path string, lines int, follow bool) error {
	if path == "" {
		if lines < 0 {
			_, err := io.Copy(stdout, os.Stdin)
			return err
		}
		tail, err := viewTailReader(os.Stdin, lines)
		for _, line := range tail {
			if _, writeErr := stdout.Write(line); writeErr != nil {
				return writeErr
			}
		}
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	if info.IsDir() {
		file.Close()
		return fmt.Errorf("%s: %w", path, syscall.EISDIR)
	}
	if follow && lines < 0 {
		lines = viewFollowDefaultLines
	}
	offset := int64(0)
	if lines >= 0 {
		if offset, err = viewTailOffset(file, info.Size(), lines); err != nil {
			file.Close()
			return err
		}
	}
	if !follow {
		defer file.Close()
		_, err := io.Copy(stdout, io.NewSectionReader(file, offset, info.Size()-offset))
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var failure error
	followViewFile(ctx, path, file, offset, func(event viewFollowEvent) bool {
		switch {
		case event.err != nil:
			failure = event.err
			return false
		case event.reset != "":
			fmt.Fprintln(stderr, T("view.reset."+event.reset, path))
		default:
			if _, err := stdout.Write(event.data); err != nil {
				failure = err
				return false
			}
		}
		return true
	})
	return failure
}

// runViewScreen은 전체 화면을 연다. 파일은 끝에서 lines줄(기본 viewKeepLines)을 읽고 그 뒤로 늘어나는 줄을 따라간다.
func runViewScreen(path string, lines int) int {
	keep := viewKeepLines
	if lines > 0 {
		keep = lines
	}
	title := path
	if path == "" {
		title = "stdin"
	}
	model := newViewScreenModel(title, keep)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var start func(program *tea.Program)
	if path == "" {
		start = func(program *tea.Program) { go viewReadStdin(ctx, program) }
	} else {
		file, err := os.Open(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, T("view.failed", err))
			return 1
		}
		info, err := file.Stat()
		if err == nil && info.IsDir() {
			err = fmt.Errorf("%s: %w", path, syscall.EISDIR)
		}
		// -n 0은 빈 화면에서 시작해 새 줄만 받는다.
		initialLines := keep
		if lines == 0 {
			initialLines = 0
		}
		var initial []byte
		if err == nil {
			initial, err = viewReadInitial(file, info.Size(), initialLines, viewScreenReadLimit)
		}
		if err != nil {
			file.Close()
			fmt.Fprintln(os.Stderr, T("view.failed", err))
			return 1
		}
		var raw []string
		raw, model.pending = viewSplitLines(nil, initial)
		model.ingest(raw, time.Now(), false)
		model.changed = info.ModTime()
		start = func(program *tea.Program) {
			go followViewFile(ctx, path, file, info.Size(), func(event viewFollowEvent) bool {
				if ctx.Err() != nil {
					return false
				}
				program.Send(viewDataMsg{event: event})
				return ctx.Err() == nil
			})
		}
	}
	program := tea.NewProgram(model, tea.WithOutput(os.Stdout))
	start(program)
	if _, err := program.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// viewReadStdin은 stdin을 읽는 대로 화면에 넘긴다. 끝나면 마지막 조각을 넣도록 eof를 보낸다.
func viewReadStdin(ctx context.Context, program *tea.Program) {
	buffer := make([]byte, viewBlockSize)
	for ctx.Err() == nil {
		count, err := os.Stdin.Read(buffer)
		if count > 0 {
			data := make([]byte, count)
			copy(data, buffer[:count])
			program.Send(viewDataMsg{event: viewFollowEvent{data: data}})
		}
		if errors.Is(err, io.EOF) {
			program.Send(viewDataMsg{eof: true})
			return
		}
		if err != nil {
			program.Send(viewDataMsg{event: viewFollowEvent{err: err}, eof: true})
			return
		}
	}
}
