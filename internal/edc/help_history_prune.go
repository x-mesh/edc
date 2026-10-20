package edc

import (
	"io"
)

func printHistoryPruneHelp(output io.Writer) {
	color := helpColor(output)
	width := 80
	if color {
		width = min(width, terminalWidth())
	}
	renderHistoryPruneHelp(output, max(20, width), color)
}

func renderHistoryPruneHelp(output io.Writer, width int, color bool) {
	render := helpRenderer{output: output, width: width, color: color}
	renderHelpHeader(output, "history prune", T("command.history.sub.prune"), width, color)
	render.panel(T("help.usage_label"), func() {
		for _, line := range []string{"edc history prune [--older-than <age>] [options] [<DB>]", "edc history prune --older-than <age> --dry-run [<DB>]", "edc history prune --older-than <age> --yes [--vacuum] [<DB>]"} {
			render.syntax(line)
		}
	})
	render.panel(T("help.examples_label"), func() {
		for _, line := range []string{"edc history prune", "edc history prune --older-than 30d --dry-run", "edc history prune --older-than 90d --command top"} {
			render.syntax(line)
		}
	})
	render.panel(T("help.options_label"), func() {
		render.options([]optionDoc{{"--older-than <age>", "history.option.older_than"}, {"--command <top|snapshot>", "history.option.prune_command"}, {"--dry-run", "history.option.dry_run"}, {"--yes", "history.option.yes"}, {"--vacuum", "history.option.vacuum"}})
	})
	for _, group := range []struct {
		title string
		notes []string
	}{{"interactive_title", []string{"retention", "keys", "period"}}, {"deletion_title", []string{"confirm", "recheck", "vacuum"}}, {"storage_title", []string{"storage"}}} {
		render.panel(T("history.prune_help."+group.title), func() {
			for _, key := range group.notes {
				render.note(T("history.prune_help." + key))
			}
			if group.title == "storage_title" {
				if path, err := defaultHistoryPath(); err == nil {
					render.note(T("history.database_path", "`"+reportIdentityValue(path)+"`"))
				}
			}
		})
	}
}
