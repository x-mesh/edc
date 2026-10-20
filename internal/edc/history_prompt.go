package edc

import (
	"database/sql"
	"os"
	"strings"
	"time"
)

// history의 trend와 export는 지표 이름과 실행 ID를 알아야 실행할 수 있다. 둘 다 DB에 기록된 값이라, 터미널에서는 빠진 값을
// 기록에서 고르게 하고 고른 값을 채운 명령을 보여 준다. 터미널이 아니면 묻지 않고 지금과 같은 usage 오류로 끝난다.

const (
	historyTopSource      = "top"
	historyExportRunLimit = 50
)

// historyCanPrompt는 선택 화면을 그릴 수 있는지다. 화면은 stdout에 그리므로 stdout이 파이프면 묻지 않는다.
func historyCanPrompt() (*os.File, bool) {
	file, ok := terminalInput(os.Stdin)
	if !ok || !isTerminal(os.Stdout) {
		return nil, false
	}
	return file, true
}

// historyTrendSources는 trend로 볼 수 있는 기록이다. top 기록이 있으면 맨 앞에 두고, 스냅샷 결과는 이름순이다.
func historyTrendSources(db *sql.DB) ([]selectItem, error) {
	var items []selectItem
	var top int
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM top_samples)").Scan(&top); err != nil {
		return nil, err
	}
	if top == 1 {
		items = append(items, selectItem{label: T("history.prompt.top_source"), value: historyTopSource})
	}
	has, err := historyHasSnapshots(db)
	if err != nil || !has {
		return items, err
	}
	probes, err := historyDistinct(db, "SELECT DISTINCT probe FROM snapshot_metrics ORDER BY probe")
	for _, probe := range probes {
		items = append(items, selectItem{label: historyTerminalText(probe), value: probe})
	}
	return items, err
}

// historyTrendKeys는 고른 기록의 숫자 지표다. top은 기록할 수 있는 호스트 지표 전부이고, 스냅샷은 기록된 키다.
func historyTrendKeys(db *sql.DB, source string) ([]string, error) {
	if source == historyTopSource {
		return historyMetricNames(historyHostMetrics), nil
	}
	return historyDistinct(db, "SELECT DISTINCT key FROM snapshot_metrics WHERE probe=? ORDER BY key", source)
}

func historyDistinct(db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// promptHistoryTrend는 빠진 --probe와 --metric을 고르게 한다. --probe를 주었으면 지표만 묻는다. top 기록을 고르면
// 호스트 전체인지 기록된 프로세스인지 묻고, --process를 주었으면 프로세스 지표만 묻는다.
func promptHistoryTrend(db *sql.DB, options *historyTrendOptions, args []string) bool {
	file, ok := historyCanPrompt()
	if !ok {
		return false
	}
	var chosen []string
	source := options.probe
	if options.process.active() {
		source = historyTopSource
	} else if source == "" {
		sources, err := historyTrendSources(db)
		if err != nil || len(sources) == 0 {
			return false
		}
		if source, err = runSelect(file, os.Stdout, newSelectModel("history.prompt.source_title", "history.prompt.source_label", sources)); err != nil {
			return false
		}
		if source == historyTopSource {
			process, ok := promptHistoryProcess(db, file)
			if !ok {
				return false
			}
			if process != "" {
				options.process, _ = parseTopProcessFilter(process)
				chosen = append(chosen, "--process", process)
			}
		}
	}
	var keys []string
	var err error
	if options.process.active() {
		keys = historyProcessMetricNames()
	} else if keys, err = historyTrendKeys(db, source); err != nil || len(keys) == 0 {
		return false
	}
	items := make([]selectItem, 0, len(keys))
	for _, key := range keys {
		items = append(items, selectItem{label: historyTerminalText(key), value: key})
	}
	metric, err := runSelect(file, os.Stdout, newSelectModel("history.prompt.metric_title", "history.prompt.metric_label", items))
	if err != nil {
		return false
	}
	if source != historyTopSource {
		options.probe = source
		if !historyHasFlag(args, "probe") {
			chosen = append(chosen, "--probe", source)
		}
	}
	options.metric = metric
	chosen = append(chosen, "--metric", metric)
	promptEcho(os.Stdout, historyEchoCommand("trend", chosen, args))
	return true
}

// promptHistoryProcess는 호스트 전체와 기록된 프로세스 가운데 하나를 고르게 한다. 호스트 전체면 빈 문자열이다.
func promptHistoryProcess(db *sql.DB, file *os.File) (string, bool) {
	names, err := historyProcessNames(db, historyProcessBySamples)
	if err != nil {
		return "", false
	}
	if len(names) == 0 {
		return "", true
	}
	items := []selectItem{{label: T("history.prompt.host_target"), value: ""}}
	for _, name := range names {
		items = append(items, selectItem{label: T("history.prompt.process_item", historyTerminalText(name.Name), name.Samples), value: name.Name})
	}
	process, err := runSelect(file, os.Stdout, newSelectModel("history.prompt.target_title", "history.prompt.target_label", items))
	return process, err == nil
}

// historyExportRuns는 내보낼 수 있는 스냅샷 실행이다. 최근 것부터 historyExportRunLimit개다.
func historyExportRuns(db *sql.DB) ([]selectItem, error) {
	has, err := historyHasSnapshots(db)
	if err != nil || !has {
		return nil, err
	}
	rows, err := db.Query("SELECT id, started_ns, sample_count FROM runs WHERE command=? ORDER BY started_ns DESC, id DESC LIMIT ?", historySnapshotCommand, historyExportRunLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []selectItem
	for rows.Next() {
		var id string
		var started int64
		var results int
		if err := rows.Scan(&id, &started, &results); err != nil {
			return nil, err
		}
		label := T("history.prompt.run_item", time.Unix(0, started).Local().Format("2006-01-02 15:04:05"), historyTerminalText(id), results)
		items = append(items, selectItem{label: label, value: id})
	}
	return items, rows.Err()
}

// promptHistoryExport는 빠진 --run을 기록된 스냅샷에서 고르게 한다.
func promptHistoryExport(db *sql.DB, args []string) (string, bool) {
	file, ok := historyCanPrompt()
	if !ok {
		return "", false
	}
	runs, err := historyExportRuns(db)
	if err != nil || len(runs) == 0 {
		return "", false
	}
	id, err := runSelect(file, os.Stdout, newSelectModel("history.prompt.run_title", "history.prompt.run_label", runs))
	if err != nil {
		return "", false
	}
	promptEcho(os.Stdout, historyEchoCommand("export", []string{"--run", id}, args))
	return id, true
}

// historyEchoCommand는 고른 option을 원래 인자 앞에 넣는다. DB 경로 같은 위치 인자 뒤의 option은 읽히지 않기 때문이다.
func historyEchoCommand(command string, chosen, args []string) string {
	words := append([]string{"edc", "history", command}, chosen...)
	words = append(words, args...)
	for index, word := range words {
		words[index] = quoteLocalShellWord(word)
	}
	return strings.Join(words, " ")
}

func historyHasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		flagName, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if strings.HasPrefix(arg, "-") && flagName == name {
			return true
		}
	}
	return false
}
