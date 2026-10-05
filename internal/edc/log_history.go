package edc

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type logHistoryOptions struct {
	Directory, File, Key, Name string
	Exact                      []string
	Failed                     bool
	Limit                      int
}

func parseLogHistoryOptions(args []string, output io.Writer) (logHistoryOptions, error) {
	options := logHistoryOptions{Limit: 20}
	set := flag.NewFlagSet("log history", flag.ContinueOnError)
	set.SetOutput(output)
	set.StringVar(&options.Directory, "dir", "", T("cli.log_history.option.dir"))
	set.StringVar(&options.File, "file", "", T("cli.log_history.option.file"))
	seenKey, seenCommand := false, false
	set.Func("key", T("cli.log_history.option.key"), func(value string) error {
		if seenKey || value == "" {
			return errors.New(T("cli.log_history.usage"))
		}
		seenKey = true
		options.Key = value
		return nil
	})
	set.Func("command", T("cli.log_history.option.command"), func(value string) error {
		if seenCommand || value == "" {
			return errors.New(T("cli.log_history.usage"))
		}
		seenCommand = true
		options.Name = value
		return nil
	})
	set.IntVar(&options.Limit, "limit", 20, T("cli.log_history.option.limit"))
	set.BoolVar(&options.Failed, "failed", false, T("cli.log_history.option.failed"))
	if err := set.Parse(args); err != nil {
		return options, err
	}
	if options.Directory != "" && options.File != "" || options.Limit < 1 || options.Limit > 1000 {
		return options, errors.New(T("cli.log_history.usage"))
	}
	if set.NArg() > 0 {
		options.Exact = set.Args()
	}
	if seenKey && seenCommand || options.Exact != nil && (seenKey || seenCommand) {
		return options, errors.New(T("cli.log_history.usage"))
	}
	if options.Exact != nil {
		key, err := commandKey(options.Exact)
		if err != nil {
			return options, errors.New(T("cli.log_history.usage"))
		}
		options.Key = key
	}
	options.Key = strings.ToLower(options.Key)
	if options.Key != "" {
		if len(options.Key) < 12 || len(options.Key) > 64 {
			return options, errors.New(T("cli.log_history.usage"))
		}
		for _, char := range options.Key {
			if !strings.ContainsRune("0123456789abcdef", char) {
				return options, errors.New(T("cli.log_history.usage"))
			}
		}
	}
	return options, nil
}

func loadLogHistory(options logHistoryOptions) (logHistorySnapshot, error) {
	if options.File != "" {
		if _, err := os.Lstat(options.File); err != nil {
			return logHistorySnapshot{}, err
		}
		return collectLogHistory([]string{options.File}), nil
	}
	root := options.Directory
	if root == "" {
		root = defaultLogDirectory()
	}
	if root == "" {
		return logHistorySnapshot{}, errors.New(T("cli.log_history.directory_unavailable"))
	}
	paths, err := historyPaths(root)
	limited := errors.Is(err, errHistoryScanLimit)
	if err != nil && !limited && !(options.Directory == "" && os.IsNotExist(err)) {
		return logHistorySnapshot{}, err
	}
	if options.Directory == "" && activeConfig.Defaults.Log.Output != nil && *activeConfig.Defaults.Log.Output != "" {
		path := *activeConfig.Defaults.Log.Output
		if _, err := os.Lstat(path); err != nil {
			if !os.IsNotExist(err) {
				return logHistorySnapshot{}, err
			}
		} else {
			paths = append(paths, path)
		}
	}
	snapshot := collectLogHistory(paths)
	if limited {
		snapshot.issue(root, "limit_error")
	}
	return snapshot, nil
}

type logHistoryGroup struct {
	Key             string
	Representative  logHistoryAttempt
	Rows            []logHistoryAttempt
	Failed, Unknown int
}

func logHistoryGroups(snapshot logHistorySnapshot, options logHistoryOptions) ([]logHistoryGroup, int) {
	indexed := map[string]*logHistoryGroup{}
	unknown := 0
	for _, row := range snapshot.Rows {
		if options.Name != "" && (len(row.Command) == 0 || filepath.Base(row.Command[0]) != options.Name) {
			continue
		}
		if row.Key == "" {
			unknown++
			continue
		}
		group := indexed[row.Key]
		if group == nil {
			group = &logHistoryGroup{Key: row.Key, Representative: row}
			indexed[row.Key] = group
		}
		if (group.Representative.Display == "name" || group.Representative.Display == "unknown") && row.Display != "name" && row.Display != "unknown" {
			group.Representative.Command = row.Command
			group.Representative.Display = row.Display
		}
		group.Rows = append(group.Rows, row)
		if row.failed() {
			group.Failed++
		}
		if row.Outcome == "UNKNOWN" {
			group.Unknown++
		}
	}
	groups := make([]logHistoryGroup, 0, len(indexed))
	for _, group := range indexed {
		if options.Failed && group.Failed == 0 {
			continue
		}
		groups = append(groups, *group)
	}
	sort.Slice(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		if !a.Rows[0].Started.Equal(b.Rows[0].Started) {
			return a.Rows[0].Started.After(b.Rows[0].Started)
		}
		return a.Key < b.Key
	})
	return groups, unknown
}

func selectHistoryGroup(groups []logHistoryGroup, key string) (logHistoryGroup, error) {
	var matches []logHistoryGroup
	for _, group := range groups {
		if strings.HasPrefix(group.Key, key) {
			matches = append(matches, group)
		}
	}
	if len(matches) == 0 {
		return logHistoryGroup{}, errors.New(T("cli.log_history.no_match"))
	}
	if len(matches) > 1 {
		keys := make([]string, len(matches))
		for i, group := range matches {
			keys[i] = group.Key
		}
		return logHistoryGroup{}, errors.New(T("cli.log_history.ambiguous", strings.Join(keys, "\n")))
	}
	return matches[0], nil
}

func printHistoryKeyList(output io.Writer, groups []logHistoryGroup) {
	fmt.Fprintln(output, T("cli.log_history.list_title"))
	for _, group := range groups {
		fmt.Fprintf(output, "%s  %s\n  %s  ·  %s\n  key %s\n", group.Key[:12], historyCommand(group.Representative), historyTime(group.Rows[0].Started), T("cli.log_history.counts", len(group.Rows), group.Failed, group.Unknown), group.Key)
	}
	fmt.Fprintln(output, T("cli.log_history.key_hint"))
}

func historyRows(group logHistoryGroup, options logHistoryOptions) []logHistoryAttempt {
	var rows []logHistoryAttempt
	for _, row := range group.Rows {
		if options.Failed && !row.failed() {
			continue
		}
		rows = append(rows, row)
		if len(rows) >= options.Limit {
			break
		}
	}
	return rows
}

func printHistoryGroup(output io.Writer, group logHistoryGroup, options logHistoryOptions) {
	fmt.Fprintf(output, "%s\nkey %s\n%s\n\n", historyCommand(group.Representative), group.Key, T("cli.log_history.time_note"))
	rows := historyRows(group, options)
	for _, row := range rows {
		exit := "—"
		if row.Exit != nil {
			exit = fmt.Sprint(*row.Exit)
		}
		fmt.Fprintf(output, "%s  %10s  %-10s  exit %-3s  %s\n", historyTime(row.Started), historyDuration(row), historyOutcome(row), exit, T("cli.log_history.attempt", row.Attempt))
		fmt.Fprintf(output, "  cwd %s\n  %s\n", reportIdentityValue(row.CWD), reportIdentityValue(row.Path))
	}
	if len(rows) == 0 {
		fmt.Fprintln(output, T("cli.log_history.no_match"))
		return
	}
	type stats struct {
		count, failed, unknown, success int
		total                           big.Int
		outcomes                        map[string]int
		low, high                       time.Duration
	}
	contexts := map[string]*stats{}
	var order []string
	for _, row := range rows {
		stat := contexts[row.CWD]
		if stat == nil {
			stat = &stats{outcomes: map[string]int{}}
			contexts[row.CWD] = stat
			order = append(order, row.CWD)
		}
		stat.count++
		stat.outcomes[row.Outcome]++
		if row.failed() {
			stat.failed++
		}
		if row.Outcome == "UNKNOWN" {
			stat.unknown++
		}
		if row.Outcome == "SUCCESS" {
			stat.success++
			stat.total.Add(&stat.total, big.NewInt(int64(row.Duration)))
			if stat.success == 1 || row.Duration < stat.low {
				stat.low = row.Duration
			}
			if row.Duration > stat.high {
				stat.high = row.Duration
			}
		}
	}
	fmt.Fprintln(output, "\n"+T("cli.log_history.summary_note", len(rows)))
	for _, cwd := range order {
		stat := contexts[cwd]
		fmt.Fprintf(output, "cwd %s  ·  %s  ·  %s\n", reportIdentityValue(cwd), T("cli.log_history.counts", stat.count, stat.failed, stat.unknown), T("cli.log_history.success_count", stat.success))
		fmt.Fprintln(output, T("cli.log_history.outcome_counts", stat.outcomes["SUCCESS"], stat.outcomes["FAIL"], stat.outcomes["TIMEOUT"], stat.outcomes["SIGNAL"], stat.outcomes["ERROR"], stat.outcomes["UNKNOWN"]))
		if stat.success > 0 {
			fmt.Fprintln(output, T("cli.log_history.timing", time.Duration(new(big.Int).Quo(&stat.total, big.NewInt(int64(stat.success))).Int64()).String(), stat.low.String(), stat.high.String()))
		}
	}
}

func runLogHistory(args []string, streams logStreams) int {
	options, err := parseLogHistoryOptions(args, streams.stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(streams.stderr, err)
		return 2
	}
	snapshot, err := loadLogHistory(options)
	if err != nil {
		fmt.Fprintln(streams.stderr, reportIdentityValue(err.Error()))
		return 2
	}
	code := 0
	if len(snapshot.Issues) > 0 {
		code = 2
		for _, issue := range snapshot.Issues {
			fmt.Fprintln(streams.stderr, issue)
		}
	}
	groupOptions := options
	if options.Key != "" {
		groupOptions.Failed = false
	}
	groups, unknown := logHistoryGroups(snapshot, groupOptions)
	notice := ""
	if unknown > 0 {
		notice = T("cli.log_history.unknown_keys", unknown)
		fmt.Fprintln(streams.stderr, notice)
	}
	if options.Key != "" {
		group, err := selectHistoryGroup(groups, options.Key)
		if err != nil {
			fmt.Fprintln(streams.stderr, err)
			if options.Exact != nil {
				fmt.Fprintln(streams.stderr, T("cli.log_history.exact_hint"))
			}
			if strings.Contains(err.Error(), T("cli.log_history.no_match")) {
				return code
			}
			return 2
		}
		printHistoryGroup(streams.stdout, group, options)
		return code
	}
	if len(groups) == 0 {
		if len(snapshot.Rows) > 0 || options.Name != "" || options.Failed {
			fmt.Fprintln(streams.stdout, T("cli.log_history.no_match"))
		} else {
			fmt.Fprintln(streams.stdout, T("cli.log_history.empty"))
		}
		return code
	}
	input, inputOK := streams.stdin.(*os.File)
	output, outputOK := streams.stdout.(*os.File)
	if inputOK && outputOK && isTerminal(input) && isTerminal(output) && os.Getenv("NO_COLOR") == "" {
		notices := append([]string{}, snapshot.Issues...)
		if notice != "" {
			notices = append(notices, notice)
		}
		if err := runLogHistoryBrowser(input, output, groups, options, notices); err != nil {
			fmt.Fprintln(streams.stderr, err)
			return 2
		}
	} else {
		printHistoryKeyList(streams.stdout, groups)
	}
	return code
}
