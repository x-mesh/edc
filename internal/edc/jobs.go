package edc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// jobs는 예약 작업과 그 마지막 결과를 보인다. svc는 지금 도는 service를 보므로, 밤에 한 번 돌고 실패한 작업은 여기서 보인다.

func init() {
	for _, probe := range []string{jobsProbeTimers, jobsProbeCron, jobsProbeLaunchd} {
		evidenceShownProbes[probe] = true
	}
}

const (
	jobsProbeTimers  = "jobs.timers"
	jobsProbeCron    = "jobs.cron"
	jobsProbeLaunchd = "jobs.launchd"
)

// scheduledJob은 예약 작업 하나다. Next와 Last가 0이면 모르는 값이다. Result가 비면 마지막 결과를 기록하지 않는 원천이다.
type scheduledJob struct {
	// Inspect는 실패한 작업을 더 볼 명령이다. 비어 있으면 edc jobs -v를 권한다.
	Name, Schedule, User, Command, Inspect string
	Next, Last                    time.Time
	Result                        string
	Failed                        bool
}

// jobsSource는 원천 하나의 결과다. Err가 있으면 그 원천은 skip이고, Unreadable은 권한 때문에 읽지 못한 파일이다.
type jobsSource struct {
	Probe      string
	Jobs       []scheduledJob
	Err        error
	Unreadable []string
	// Note는 원천의 한계다. cron은 마지막 결과를 남기지 않는다.
	Note string
}

var errJobsUnsupported = errors.New("unsupported")

// verbose가 아니면 실패가 있을 때 실패한 작업만 근거로 남긴다. 실패 상자에 정상 작업까지 쌓이면 원인이 묻힌다.
func jobsResult(source jobsSource, now time.Time, verbose bool) Result {
	started := time.Now()
	if errors.Is(source.Err, errJobsUnsupported) {
		return unsupported(source.Probe, T("jobs.skip.unsupported"))
	}
	if source.Err != nil {
		return Result{Probe: source.Probe, Status: StatusSkip, StartedAt: started.UTC(), Summary: infoSingleLine(source.Err.Error())}
	}
	jobs := append([]scheduledJob(nil), source.Jobs...)
	// 실패한 작업을 먼저, 그다음 곧 돌 작업을 먼저 둔다.
	sort.SliceStable(jobs, func(a, b int) bool {
		if jobs[a].Failed != jobs[b].Failed {
			return jobs[a].Failed
		}
		if jobs[a].Next.IsZero() != jobs[b].Next.IsZero() {
			return !jobs[a].Next.IsZero()
		}
		return jobs[a].Next.Before(jobs[b].Next)
	})
	var failed []string
	result := Result{Probe: source.Probe, Status: StatusPass, StartedAt: started.UTC()}
	for _, job := range jobs {
		if job.Failed {
			failed = append(failed, job.Name)
		}
		if job.Failed || verbose || !jobsAnyFailed(jobs) {
			result.Evidence = append(result.Evidence, Evidence{Label: job.Name, Value: jobsLine(job, now)})
		}
	}
	result.Summary = T("jobs.summary", len(jobs))
	if len(failed) > 0 {
		result.Status = StatusFail
		result.Summary = T("jobs.failed", len(failed), len(jobs), checkNames(failed))
		result.Error = &DiagnosticError{Kind: "jobs", Message: result.Summary}
		result.Next = jobs[0].Inspect
		if result.Next == "" && !verbose && len(failed) < len(jobs) {
			result.Next = "edc jobs -v"
		}
	}
	if source.Note != "" {
		result.Warnings = append(result.Warnings, source.Note)
	}
	if len(source.Unreadable) > 0 {
		result.Warnings = append(result.Warnings, T("jobs.unreadable", len(source.Unreadable), checkNames(source.Unreadable)))
	}
	result.DurationMS = time.Since(started).Milliseconds()
	return result
}

func jobsAnyFailed(jobs []scheduledJob) bool {
	for _, job := range jobs {
		if job.Failed {
			return true
		}
	}
	return false
}

// jobsLine은 일정, 다음과 마지막 실행, 결과, 명령을 한 칸에 적는다. 모르는 값은 뺀다.
func jobsLine(job scheduledJob, now time.Time) string {
	var parts []string
	if job.Schedule != "" {
		parts = append(parts, job.Schedule)
	}
	if !job.Next.IsZero() {
		parts = append(parts, T("jobs.next", jobsDuration(job.Next.Sub(now))))
	}
	if !job.Last.IsZero() {
		parts = append(parts, T("jobs.last", jobsDuration(now.Sub(job.Last))))
	}
	if job.Result != "" {
		parts = append(parts, job.Result)
	}
	line := strings.Join(parts, " · ")
	command := job.Command
	if job.User != "" {
		command = job.User + ": " + command
	}
	if command != "" {
		line += "\n" + command
	}
	return line
}

// jobsDuration은 1분 이상은 분 단위로, 그보다 짧으면 초 단위로 적는다. 분으로 자르면 곧 돌 작업이 0s로 보인다.
func jobsDuration(value time.Duration) string {
	if value >= time.Minute {
		return eventsDurationText(value.Truncate(time.Minute))
	}
	if value < time.Second {
		value = time.Second
	}
	return eventsDurationText(value.Truncate(time.Second))
}

// parseTimerUnits는 list-units --plain --no-legend의 첫 열에서 timer unit을 고른다.
func parseTimerUnits(output string) []string {
	var timers []string
	for _, line := range strings.Split(output, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && strings.HasSuffix(fields[0], ".timer") {
			timers = append(timers, fields[0])
		}
	}
	return timers
}

// parseSystemctlShowBlocks는 systemctl show가 unit마다 빈 줄로 나눠 낸 key=value 묶음이다.
func parseSystemctlShowBlocks(output string) []map[string]string {
	var blocks []map[string]string
	for _, block := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n\n") {
		values := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			if key, value, ok := strings.Cut(line, "="); ok {
				values[key] = value
			}
		}
		if values["Id"] != "" {
			blocks = append(blocks, values)
		}
	}
	return blocks
}

// parseSystemdTime은 systemctl show의 "Wed 2026-11-04 05:24:28 KST"다. systemctl은 edc와 같은 환경에서 돌아 지역 시간으로
// 적으므로 시간대 약어는 떼고 지역 시간으로 읽는다. 약어로 읽으면 Go가 모르는 약어를 UTC로 본다. n/a나 빈 값은 모르는 값이다.
func parseSystemdTime(value string, location *time.Location) time.Time {
	fields := strings.Fields(value)
	if len(fields) < 3 {
		return time.Time{}
	}
	parsed, err := time.ParseInLocation("Mon 2006-01-02 15:04:05", strings.Join(fields[:3], " "), location)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// timerJobs는 timer와 그 timer가 깨우는 service의 show 결과를 합친다. 결과가 success가 아니면 실패다.
func timerJobs(timers, services []map[string]string, location *time.Location) []scheduledJob {
	byService := map[string]map[string]string{}
	for _, service := range services {
		byService[service["Id"]] = service
	}
	var jobs []scheduledJob
	for _, timer := range timers {
		job := scheduledJob{Name: timer["Id"], Command: timer["Unit"], Next: parseSystemdTime(timer["NextElapseUSecRealtime"], location), Last: parseSystemdTime(timer["LastTriggerUSec"], location)}
		service := byService[timer["Unit"]]
		switch result := service["Result"]; {
		case job.Last.IsZero() && result == "success":
			// 한 번도 돌지 않은 service의 Result도 success다. 결과로 적지 않는다.
		case result == "success":
			job.Result = "success"
		case result != "":
			job.Failed = true
			job.Result = result
			job.Inspect = "journalctl -u " + timer["Unit"] + " -n 20 --no-pager"
			if status := service["ExecMainStatus"]; status != "" && status != "0" {
				job.Result += " (status " + status + ")"
			}
		}
		jobs = append(jobs, job)
	}
	return jobs
}

// parseCrontab은 crontab 파일 하나를 읽는다. system이면 /etc/crontab처럼 여섯째 칸이 사용자다. 환경 변수 줄과 주석은 건너뛴다.
func parseCrontab(name, text, owner string, system bool) []scheduledJob {
	var jobs []scheduledJob
	for number, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		scheduleFields := 5
		if strings.HasPrefix(fields[0], "@") {
			scheduleFields = 1
		} else if key, _, isAssignment := strings.Cut(fields[0], "="); isAssignment && key != "" {
			continue
		}
		need := scheduleFields + 1
		if system {
			need++
		}
		if len(fields) < need {
			continue
		}
		job := scheduledJob{Name: fmt.Sprintf("%s:%d", name, number+1), Schedule: strings.Join(fields[:scheduleFields], " "), User: owner}
		rest := fields[scheduleFields:]
		if system {
			job.User, rest = rest[0], rest[1:]
		}
		// 명령 안의 공백을 살리려고 원래 줄에서 일정과 사용자 칸 뒤를 그대로 자른다.
		job.Command = cronCommandAfter(line, len(fields)-len(rest))
		jobs = append(jobs, job)
	}
	return jobs
}

func cronCommandAfter(line string, skip int) string {
	rest := line
	for range skip {
		rest = strings.TrimLeft(rest, " \t")
		if index := strings.IndexAny(rest, " \t"); index >= 0 {
			rest = rest[index:]
		} else {
			return ""
		}
	}
	return strings.TrimSpace(rest)
}

// readCrontabs는 system crontab, cron.d, 사용자 crontab, run-parts 디렉터리를 읽는다. 사용자 crontab은 root만 읽는다.
func readCrontabs(root string) jobsSource {
	source := jobsSource{Probe: jobsProbeCron, Note: T("jobs.cron.note")}
	read := func(path string) (string, bool) {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrPermission) {
			source.Unreadable = append(source.Unreadable, path)
		}
		return string(data), err == nil
	}
	join := func(parts ...string) string { return filepath.Join(append([]string{root}, parts...)...) }
	// 시험은 root를 임시 디렉터리로 바꾼다. 화면에는 host의 경로로 적는다.
	shown := func(path string) string {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return path
		}
		return "/" + filepath.ToSlash(relative)
	}
	found := false
	systemFiles, _ := filepath.Glob(join("etc", "cron.d", "*"))
	for _, path := range append([]string{join("etc", "crontab")}, systemFiles...) {
		if strings.HasPrefix(filepath.Base(path), ".") {
			continue
		}
		if text, ok := read(path); ok {
			found = true
			source.Jobs = append(source.Jobs, parseCrontab(shown(path), text, "", true)...)
		}
	}
	// Debian은 crontabs 아래에, RHEL은 cron 아래에 사용자 이름의 파일을 둔다.
	for _, directory := range []string{join("var", "spool", "cron", "crontabs"), join("var", "spool", "cron")} {
		entries, err := os.ReadDir(directory)
		if errors.Is(err, os.ErrPermission) {
			source.Unreadable = append(source.Unreadable, directory)
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			path := filepath.Join(directory, entry.Name())
			if text, ok := read(path); ok {
				found = true
				source.Jobs = append(source.Jobs, parseCrontab(shown(path), text, entry.Name(), false)...)
			}
		}
	}
	for _, period := range []string{"hourly", "daily", "weekly", "monthly"} {
		entries, err := os.ReadDir(join("etc", "cron."+period))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || entry.Name() == "README" {
				continue
			}
			found = true
			source.Jobs = append(source.Jobs, scheduledJob{Name: shown(filepath.Join(root, "etc", "cron."+period, entry.Name())), Schedule: "cron." + period})
		}
	}
	if !found && len(source.Unreadable) == 0 {
		source.Err = errors.New(T("jobs.cron.none"))
	}
	return source
}

// launchdJob은 launchd plist 하나다. StartInterval이나 StartCalendarInterval이 없는 job은 예약 작업이 아니라 뺀다.
func launchdJob(path string, plist map[string]any) (scheduledJob, bool) {
	label, _ := plist["Label"].(string)
	schedule := ""
	if interval, ok := plist["StartInterval"].(int64); ok && interval > 0 {
		schedule = T("jobs.launchd.interval", eventsDurationText(time.Duration(interval)*time.Second))
	}
	if calendar, ok := plist["StartCalendarInterval"]; ok {
		schedule = launchdCalendar(calendar)
	}
	if label == "" || schedule == "" {
		return scheduledJob{}, false
	}
	command, _ := plist["Program"].(string)
	if arguments, ok := plist["ProgramArguments"].([]any); ok && command == "" && len(arguments) > 0 {
		var words []string
		for _, argument := range arguments {
			if word, ok := argument.(string); ok {
				words = append(words, word)
			}
		}
		command = strings.Join(words, " ")
	}
	return scheduledJob{Name: label, Schedule: schedule, Command: command + "\n" + path}, true
}

// launchdCalendar는 StartCalendarInterval의 dict 하나나 배열을 "Hour=3 Minute=0"처럼 적는다. 빠진 칸은 매번이다.
func launchdCalendar(value any) string {
	entries, ok := value.([]any)
	if !ok {
		entries = []any{value}
	}
	var texts []string
	for _, entry := range entries {
		fields, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		var parts []string
		for _, key := range []string{"Month", "Day", "Weekday", "Hour", "Minute"} {
			if number, ok := fields[key].(int64); ok {
				parts = append(parts, fmt.Sprintf("%s=%d", key, number))
			}
		}
		if len(parts) == 0 {
			parts = append(parts, "every minute")
		}
		texts = append(texts, strings.Join(parts, " "))
	}
	return strings.Join(texts, "; ")
}

// applyLaunchctlStatus는 launchctl list의 마지막 종료 상태를 붙인다. 양수 종료 코드는 실패이고, 신호로 끝난 음수는 check와
// 같이 실패로 보지 않는다. 목록에 없는 job은 이 domain에 올라가지 않은 것이다.
func applyLaunchctlStatus(jobs []scheduledJob, output string) {
	status := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] != "PID" {
			status[strings.Join(fields[2:], " ")] = fields[1]
		}
	}
	for index := range jobs {
		code, loaded := status[jobs[index].Name]
		switch number, err := strconv.Atoi(code); {
		case !loaded:
			jobs[index].Result = T("jobs.launchd.not_loaded")
		case err == nil && number > 0:
			jobs[index].Failed, jobs[index].Result = true, fmt.Sprintf("exit %d", number)
		case err == nil && number < 0:
			jobs[index].Result = fmt.Sprintf("signal %d", -number)
		default:
			jobs[index].Result = "exit 0"
		}
	}
}

func runJobs(args []string, version string) int {
	options := configuredCommon(15 * time.Second)
	set := newFlagSet("jobs")
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() > 0 {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc jobs [options]"))
		return 2
	}
	started := time.Now()
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	now := time.Now()
	var results []Result
	for _, source := range hostJobsSources(ctx) {
		results = append(results, jobsResult(source, now, options.verbose))
	}
	return emit(options, buildReport(version, started, nil, results, options.redact))
}
