package edc

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type logPruneOptions struct {
	history              logHistoryOptions
	olderThan            time.Duration
	dryRun, yes, verbose bool
}

// parseLogAge accepts Go durations and the day and week units that a retention
// period is usually written in, such as 30d or 2w.
func parseLogAge(text string) (time.Duration, error) {
	for suffix, unit := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if number, ok := strings.CutSuffix(text, suffix); ok {
			count, err := strconv.ParseFloat(number, 64)
			if err != nil || count <= 0 {
				return 0, errors.New(T("cli.log_prune.usage"))
			}
			return time.Duration(count * float64(unit)), nil
		}
	}
	age, err := time.ParseDuration(text)
	if err != nil || age <= 0 {
		return 0, errors.New(T("cli.log_prune.usage"))
	}
	return age, nil
}

func parseLogPruneOptions(args []string, output io.Writer) (logPruneOptions, error) {
	var options logPruneOptions
	var age string
	set := newFlagSet("log prune")
	set.SetOutput(output)
	set.StringVar(&age, "older-than", "", T("cli.log_prune.option.older_than"))
	set.StringVar(&options.history.Directory, "dir", "", T("cli.log_history.option.dir"))
	set.StringVar(&options.history.File, "file", "", T("cli.log_history.option.file"))
	set.StringVar(&options.history.Name, "command", "", T("cli.log_prune.option.command"))
	set.StringVar(&options.history.Key, "key", "", T("cli.log_history.option.key"))
	set.BoolVar(&options.dryRun, "dry-run", false, T("cli.log_prune.option.dry_run"))
	set.BoolVar(&options.yes, "yes", false, T("cli.log_prune.option.yes"))
	set.BoolVar(&options.verbose, "v", false, T("cli.log_prune.option.verbose"))
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return options, err
		}
		return options, flagReported(err)
	}
	if set.NArg() > 0 || age == "" || options.history.Directory != "" && options.history.File != "" {
		return options, errors.New(T("cli.log_prune.usage"))
	}
	var err error
	options.olderThan, err = parseLogAge(age)
	options.history.Key = strings.ToLower(options.history.Key)
	if strings.Trim(options.history.Key, "0123456789abcdef") != "" {
		return options, errors.New(T("cli.log_prune.usage"))
	}
	return options, err
}

// pruneTargets selects the finished runs that started before the cutoff. A run
// without a command key is selected only when no key or command filter is set.
func pruneTargets(rows []logHistoryAttempt, options logPruneOptions, cutoff time.Time) []logHistoryAttempt {
	var targets []logHistoryAttempt
	for _, row := range rows {
		if row.Outcome == historyOutcomeRunning || row.Started.IsZero() || !row.Started.Before(cutoff) {
			continue
		}
		if options.history.Key != "" && (row.Key == "" || !strings.HasPrefix(row.Key, options.history.Key)) {
			continue
		}
		if options.history.Name != "" && (len(row.Command) == 0 || filepath.Base(row.Command[0]) != options.history.Name) {
			continue
		}
		targets = append(targets, row)
	}
	return targets
}

const pruneSummaryLimit = 10

type pruneGroup struct {
	name        string
	runs        int
	bytes       uint64
	keys        map[string]bool
	first, last time.Time
	directories map[string]bool
}

// pruneSummary groups the runs by command name, as the default directory keeps
// one folder per name, and names the directory once in the first line. -v adds
// every file.
func pruneSummary(plan logHistoryDeletion, defaultRoot string, verbose, dryRun bool) []string {
	var paths []string
	for _, family := range plan.families {
		paths = append(paths, family.path)
	}
	base := pruneBase(paths, defaultRoot)
	header := "cli.log_prune.plan"
	if dryRun {
		header = "cli.log_prune.dry_plan"
	}
	lines := []string{T(header, plan.Runs, plan.Files, formatSizeShort(plan.Bytes), reportIdentityValue(base))}
	if base == filepath.Dir(base) {
		lines[0] = T(header+"_total", plan.Runs, plan.Files, formatSizeShort(plan.Bytes))
	}
	groups := map[string]*pruneGroup{}
	for _, family := range plan.families {
		for index, row := range family.rows {
			name := T("cli.log_history.hidden")
			if len(row.Command) > 0 {
				name = filepath.Base(row.Command[0])
			}
			group := groups[name]
			if group == nil {
				group = &pruneGroup{name: name, keys: map[string]bool{}, directories: map[string]bool{}}
				groups[name] = group
			}
			group.runs++
			// A file belongs to the command of its first run, so the sizes add up to the total.
			if index == 0 {
				group.bytes += family.bytes
			}
			group.directories[filepath.Dir(family.path)] = true
			if row.Key != "" {
				group.keys[row.Key] = true
			}
			if group.first.IsZero() || row.Started.Before(group.first) {
				group.first = row.Started
			}
			if row.Started.After(group.last) {
				group.last = row.Started
			}
		}
	}
	ordered := make([]*pruneGroup, 0, len(groups))
	nameWidth := 0
	for _, group := range groups {
		ordered = append(ordered, group)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].runs != ordered[j].runs {
			return ordered[i].runs > ordered[j].runs
		}
		return ordered[i].name < ordered[j].name
	})
	shown := ordered[:min(len(ordered), pruneSummaryLimit)]
	for _, group := range shown {
		nameWidth = max(nameWidth, min(24, len([]rune(group.name))))
	}
	for _, group := range shown {
		line := "  " + historyCell(reportIdentityValue(group.name), nameWidth, false) + "  " + historyCell(T("cli.log_prune.runs", group.runs), 10, true) + "  " + pruneRange(group.first, group.last) + "  " + historyCell(formatSizeShort(group.bytes), 9, true) + "  " + pruneLocation(group.directories, base)
		if len(group.keys) > 1 {
			line += "  " + T("cli.log_prune.argument_sets", len(group.keys))
		}
		lines = append(lines, line)
	}
	if hidden := len(ordered) - len(shown); hidden > 0 {
		lines = append(lines, "  "+T("cli.log_prune.more", hidden))
	}
	if plan.Shared+plan.Running > 0 {
		lines = append(lines, T("cli.log_history.delete_kept", plan.Shared, plan.Running))
	}
	if verbose {
		for _, family := range plan.families {
			lines = append(lines, "  "+reportIdentityValue(family.path)+"  "+T("cli.log_prune.runs", family.runs))
		}
	}
	return lines
}

// pruneBase is the default log directory when every file is under it, and
// otherwise the deepest directory that holds them all.
func pruneBase(paths []string, defaultRoot string) string {
	under := defaultRoot != ""
	for _, path := range paths {
		if relative, err := filepath.Rel(defaultRoot, path); err != nil || strings.HasPrefix(relative, "..") {
			under = false
		}
	}
	if under {
		return defaultRoot
	}
	base := ""
	for _, path := range paths {
		directory := filepath.Dir(path)
		if base == "" {
			base = directory
			continue
		}
		for base != filepath.Dir(base) {
			if relative, err := filepath.Rel(base, directory); err == nil && !strings.HasPrefix(relative, "..") {
				break
			}
			base = filepath.Dir(base)
		}
	}
	return base
}

func pruneLocation(directories map[string]bool, base string) string {
	if len(directories) != 1 {
		return T("cli.log_prune.folders", len(directories))
	}
	for directory := range directories {
		relative, err := filepath.Rel(base, directory)
		if err != nil || strings.HasPrefix(relative, "..") || base == filepath.Dir(base) {
			return reportIdentityValue(directory) + "/"
		}
		if relative == "." {
			return "./"
		}
		return reportIdentityValue(relative) + "/"
	}
	return ""
}

func pruneRange(first, last time.Time) string {
	first, last = first.Local(), last.Local()
	switch {
	case first.Equal(last):
		return first.Format("2006-01-02 15:04") + "        "
	case first.Format("2006-01-02") == last.Format("2006-01-02"):
		return first.Format("2006-01-02 15:04") + " – " + last.Format("15:04")
	default:
		return historyCell(first.Format("2006-01-02")+" – "+last.Format("2006-01-02"), 24, false)
	}
}

func runLogPrune(args []string, streams logStreams) int {
	options, err := parseLogPruneOptions(args, streams.stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if errors.Is(err, errFlagReported) {
		return 2
	}
	if err != nil {
		fmt.Fprintln(streams.stderr, err)
		return 2
	}
	options.history.oldestFirst = true
	snapshot, err := loadLogHistory(options.history, nil)
	if err != nil {
		fmt.Fprintln(streams.stderr, reportIdentityValue(err.Error()))
		return 2
	}
	for _, notice := range snapshot.Notices {
		fmt.Fprintln(streams.stderr, notice)
	}
	code := 0
	for _, issue := range snapshot.Issues {
		fmt.Fprintln(streams.stderr, issue)
		code = 2
	}
	plan := planLogHistoryDeletion(pruneTargets(snapshot.Rows, options, time.Now().Add(-options.olderThan)))
	if plan.Runs == 0 {
		fmt.Fprintln(streams.stdout, T("cli.log_prune.none"))
		if plan.Shared+plan.Running > 0 {
			fmt.Fprintln(streams.stdout, T("cli.log_history.delete_kept", plan.Shared, plan.Running))
		}
		return code
	}
	for _, line := range pruneSummary(plan, defaultLogDirectory(), options.verbose, options.dryRun) {
		fmt.Fprintln(streams.stdout, line)
	}
	if options.dryRun {
		fmt.Fprintln(streams.stdout, T("cli.log_prune.dry_note"))
		return code
	}
	if !options.yes {
		input, inputOK := streams.stdin.(*os.File)
		output, outputOK := streams.stdout.(*os.File)
		if !inputOK || !outputOK || !isTerminal(input) || !isTerminal(output) {
			fmt.Fprintln(streams.stderr, T("cli.log_prune.yes_required"))
			return 2
		}
		fmt.Fprint(streams.stdout, T("cli.log_prune.confirm"))
		answer, _ := bufio.NewReader(input).ReadString('\n')
		if strings.TrimSpace(strings.ToLower(answer)) != "y" {
			return code
		}
	}
	deleted, err := plan.execute()
	// The summary is written for the runs that were really removed, so a log
	// that changed and was kept is not counted twice later.
	if options.history.Directory == "" && options.history.File == "" {
		if statsErr := saveLogHistoryRollup(defaultLogDirectory(), deleted.deleted); statsErr != nil {
			fmt.Fprintln(streams.stderr, T("cli.log_prune.stats_failed", reportIdentityValue(firstLine(statsErr.Error()))))
		}
	}
	fmt.Fprintln(streams.stdout, T("cli.log_history.delete_done", deleted.Runs, deleted.Files, formatSizeShort(deleted.Bytes)))
	if skipped := deleted.Shared + deleted.Running; skipped > 0 {
		fmt.Fprintln(streams.stdout, T("cli.log_history.delete_skipped", skipped))
	}
	if err != nil {
		fmt.Fprintln(streams.stderr, T("cli.log_history.delete_failed", reportIdentityValue(err.Error())))
		return 2
	}
	return code
}
