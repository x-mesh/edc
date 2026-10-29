package edc

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// optionDoc은 상세 화면에 그리는 option 한 줄이다.
// flag는 어느 언어에서나 같으므로 코드에 두고, 설명만 locale에서 가져온다.
type optionDoc struct {
	flag string
	key  string
}

func (option optionDoc) detail() string {
	if option.key == "" {
		return ""
	}
	return T(option.key)
}

// subcommandDoc은 하위 명령 하나다. 설명은 locale의 command.<명령>.sub.<name>에 둔다.
type subcommandDoc struct {
	name string
	form string
}

// commandDoc은 명령 하나의 뼈대다. 이름과 문법은 코드에, 설명은 locale에 둔다.
// 예시는 어느 언어에서나 같은 명령이므로 코드에 둔다.
type commandDoc struct {
	name        string
	group       string
	usage       []string
	options     []optionDoc
	usesCommon  bool
	subcommands []subcommandDoc
	examples    []string
}

func (doc commandDoc) summary() string { return T("command." + doc.name + ".summary") }

func (doc commandDoc) notes() []string {
	notes := TList("command." + doc.name + ".notes")
	if doc.name == "history" || doc.name == "snapshot" {
		if path, err := defaultHistoryPath(); err == nil {
			notes = append(notes, T("history.database_path", "`"+reportIdentityValue(path)+"`"))
		}
	}
	return notes
}

// subcommandChoices는 선택기와 usage 오류에 쓰는 하위 명령 이름이다. 하위 명령을 실행하는 run* 함수의 분기는
// 따로 적혀 있고, completion script와의 일치는 TestCompletionScriptsListThePickerSubcommands가 확인한다.
func subcommandChoices(name string) []string {
	doc, _ := findCommandDoc(name)
	names := make([]string, 0, len(doc.subcommands))
	for _, sub := range doc.subcommands {
		names = append(names, sub.name)
	}
	return names
}

// subcommandUsage는 "edc dns <lookup|compare|config>"처럼 하위 명령을 나열한 usage 앞부분이다.
func subcommandUsage(name string) string {
	return "edc " + name + " <" + strings.Join(subcommandChoices(name), "|") + ">"
}

func (doc commandDoc) subcommandKey(name string) string {
	return "command." + doc.name + ".sub." + name
}

func (doc commandDoc) subcommandLines() []optionDoc {
	lines := make([]optionDoc, 0, len(doc.subcommands))
	for _, sub := range doc.subcommands {
		lines = append(lines, optionDoc{sub.form, doc.subcommandKey(sub.name)})
	}
	return lines
}

// helpGroups는 첫 화면의 그룹 순서다. 값은 locale의 help.group 아래 키다.
var helpGroups = []string{"diagnose", "observe", "host", "record", "tool"}

var commonOptionDocs = []optionDoc{
	{"--timeout 15s", "option.timeout"},
	{"--json <path|->", "option.json"},
	{"-v, --verbose", "option.verbose"},
	{"--redact", "option.redact"},
}

// commandDocs는 help와 completion이 함께 읽는 한 벌의 명령 목록이다.
// 설명을 여기 한 곳에만 두어 두 출력이 어긋나지 않게 한다.
var commandDocs = []commandDoc{
	{
		name: "doctor", group: "diagnose",
		usage:      []string{"edc doctor [--profile default|full] [--all-ips] [options] <host|URL>"},
		options:    []optionDoc{{"--profile default|full", "command.doctor.option.profile"}, {"--all-ips", "command.doctor.option.all_ips"}},
		usesCommon: true,
	},
	{
		name: "dns", group: "diagnose",
		usage:      []string{"edc dns lookup [options] <host>", "edc dns compare [--resolver IP] [options] <host>", "edc dns config [options]"},
		options:    []optionDoc{{"--resolver IP[:port]", "command.dns.option.resolver"}},
		usesCommon: true,
		subcommands: []subcommandDoc{
			{"lookup", "edc dns lookup <host>"},
			{"compare", "edc dns compare <host>"},
			{"config", "edc dns config"},
		},
	},
	{
		name: "tcp", group: "diagnose",
		usage:      []string{"edc tcp check [options] <host:port>"},
		usesCommon: true,
	},
	{
		name: "tls", group: "diagnose",
		usage:      []string{"edc tls check [--min-days N] [options] <host:port>"},
		options:    []optionDoc{{"--min-days N", "command.tls.option.min_days"}},
		usesCommon: true,
	},
	{
		name: "http", group: "diagnose",
		usage: []string{
			"edc http check [--expect-status N] [-H 'Name: value'] [options] <URL>",
			"edc http check --body <path|-> [options] <URL>",
		},
		options: []optionDoc{
			{"--expect-status N", "command.http.option.expect_status"},
			{"-H, --header 'Name: value'", "command.http.option.header"},
			{"--body <path|->", "command.http.option.body"},
		},
		usesCommon: true,
	},
	{
		name: "net", group: "diagnose",
		usage: []string{
			"edc net interfaces [options]",
			"edc net route [options] <host>",
			"edc net ping [options] <host>",
			"edc net trace [options] <host>",
		},
		usesCommon: true,
		subcommands: []subcommandDoc{
			{"interfaces", "edc net interfaces"},
			{"route", "edc net route <host>"},
			{"ping", "edc net ping <host>"},
			{"trace", "edc net trace <host>"},
		},
	},
	{
		name: "audit", group: "diagnose",
		usage:      []string{"edc audit [options]"},
		usesCommon: true,
	},
	{
		name: "check", group: "diagnose",
		usage: []string{"edc check [options] [routine]", "edc check --group <name> [--routine NAME] [--inventory FILE] [options]"},
		options: []optionDoc{
			{"--routine NAME", "command.check.option.routine"},
			{"--group NAME", "command.check.option.group"},
			{"--inventory FILE", "command.check.option.inventory"},
		},
		usesCommon: true,
	},
	{
		name: "events", group: "diagnose",
		usage:      []string{"edc events [--since 6h] [options]"},
		options:    []optionDoc{{"--since 6h", "command.events.option.since"}},
		usesCommon: true,
	},
	{
		name: "route", group: "host",
		usage: []string{
			"edc route check [options]",
			"edc route switch --to <name> [options]",
			"edc route status [options]",
			"edc route rollback --state <file>",
		},
		options: []optionDoc{
			{"--to <name>", "route.flag.to"},
			{"--seconds N", "route.flag.seconds"},
			{"--state <file>", "route.flag.state"},
			{"--exits <file>", "route.flag.exits"},
			{"--force", "route.flag.force"},
			{"--yes", "route.flag.yes"},
			{"-n, --dry-run", "route.flag.dry_run"},
		},
		usesCommon: true,
		subcommands: []subcommandDoc{
			{"check", "edc route check"},
			{"switch", "edc route switch --to <name>"},
			{"status", "edc route status"},
			{"rollback", "edc route rollback [--state <file>]"},
		},
	},
	{
		name: "disk", group: "host",
		usage: []string{
			"edc disk check [mount]",
			"edc disk grow <mount> [options]",
		},
		options: []optionDoc{
			{"-n, --dry-run", "disk.flag.dry_run"},
			{"--yes", "disk.flag.yes"},
		},
		usesCommon: true,
		subcommands: []subcommandDoc{
			{"check", "edc disk check [mount]"},
			{"grow", "edc disk grow <mount>"},
		},
	},
	{
		name: "change", group: "host",
		usage: []string{
			"edc change apply --kind authorized-keys|iptables [options]",
			"edc change status [options]",
			"edc change confirm --state <file>",
			"edc change rollback --state <file>",
		},
		options: []optionDoc{
			{"--kind authorized-keys|iptables", "change.flag.kind"},
			{"--path <authorized_keys>", "change.flag.path"},
			{"--content-file <file>", "change.flag.content_file"},
			{"--rules-file <file>", "change.flag.rules_file"},
			{"--seconds N", "change.flag.seconds"},
			{"--state <file>", "change.flag.state"},
			{"--yes", "change.flag.yes"},
		},
		usesCommon: true,
		subcommands: []subcommandDoc{
			{"apply", "edc change apply --kind <kind>"},
			{"status", "edc change status"},
			{"confirm", "edc change confirm [--state <file>]"},
			{"rollback", "edc change rollback [--state <file>]"},
		},
	},
	{
		name: "top", group: "observe",
		usage: []string{"edc top [--interval 1s] [--count N] [--json <path|->]", "edc top --process <filter> [--full] [-d] [--interval 1s] [--json <path|->]", "edc top setup"},
		options: []optionDoc{
			{"--interval 1s", "command.top.option.interval"},
			{"-w, --write [path]", "command.top.option.write"},
			{"--count N", "command.top.option.count"},
			{"--no-header", "command.top.option.no_header"},
			{"--process <filter>", "command.top.option.process"},
			{"--top 10", "command.top.option.top"},
			{"--sort cpu", "command.top.option.sort"},
			{"--full", "command.top.option.full"},
			{"--split [list]", "command.top.option.split"},
			{"--view <name>", "command.top.option.view"},
			{"--panels <list>", "command.top.option.panels"},
			{"--columns <list>", "command.top.option.columns"},
			{"--interface <list>", "command.top.option.interface"},
			{"-d, --detail", "command.top.option.detail"},
			{"--ebpf", "command.top.option.ebpf"},
			{"--json <path|->", "command.top.option.json"},
		},
	},
	{
		name: "history", group: "record",
		usage: []string{"edc history [--from <time>] [--to <time>] [--run <id>] [--limit <n>] [DB]", "edc history list [options] [DB]", "edc history top [options] [DB]", "edc history process [options] [DB]", "edc history overview [--run <id>] [--from <time> --to <time>] [DB]", "edc history export --run <id> [--output <file|->] [DB]", "edc history trend --metric <name> [--probe <id> | --process <filter>] [DB]", "edc history growth [--root <path> | --process <filter>] [DB]", "edc history prune [--older-than <age>] [options] [DB]", "edc history log [edc log history options]"},
		options: []optionDoc{
			{"--run <id>", "history.option.run"},
			{"--from <RFC3339>", "history.option.from"},
			{"--to <RFC3339>", "history.option.to"},
			{"--process <filter>", "command.top.option.process"},
			{"--metric <name>", "history.option.metric"},
			{"--probe <id>", "history.option.probe"},
			{"--by day", "history.option.by"},
			{"--each", "history.option.each"},
			{"--root <path>", "history.option.root"},
			{"--min <number>", "history.option.min"},
			{"--max <number>", "history.option.max"},
			{"--limit 200", "history.option.limit"},
			{"--json <path|->", "history.option.json"},
			{"--output <file|->", "history.option.output"},
			{"--older-than 30d", "history.option.older_than"},
			{"--command <kind>", "history.option.prune_command"},
			{"--dry-run", "history.option.dry_run"},
			{"--yes", "history.option.yes"},
			{"--vacuum", "history.option.vacuum"},
		},
		subcommands: []subcommandDoc{
			{"list", "edc history list [options] [DB]"},
			{"top", "edc history top [options] [DB]"},
			{"process", "edc history process [options] [DB]"},
			{"overview", "edc history overview [--run <id>] [--from <time> --to <time>] [DB]"},
			{"export", "edc history export --run <id> [DB]"},
			{"trend", "edc history trend [options] [DB]"},
			{"growth", "edc history growth [options] [DB]"},
			{"prune", "edc history prune [options] [DB]"},
			{"log", "edc history log [<command>]"},
		},
		examples: []string{
			"edc history list",
			"edc history top --metric memory_pct --min 90",
			"edc history process --process nginx",
			"edc history trend --probe info.disk:/ --metric used_bytes",
			"edc history trend --process postgres --metric rss_bytes",
			"edc history growth --root /var",
			"edc history growth --process postgres",
			"edc history prune --older-than 90d --dry-run",
		},
	},
	{
		name: "watch", group: "observe",
		usage: []string{"edc watch http [-i seconds] [--duration 1m] [options] <host|URL>", fsWatchUsage},
		options: []optionDoc{
			{"-i, --interval 0.1", "command.watch.option.interval"},
			{"--duration 1m", "command.watch.option.duration"},
			{"--expect-status N", "command.http.option.expect_status"},
			{"-r, --recursive", "watchfs.option.recursive"},
			{"--driver auto", "watchfs.option.driver"},
			{"--event <events>", "watchfs.option.event"},
			{"--match <glob>", "watchfs.option.match"},
			{"--exec <command>", "watchfs.option.exec"},
			{"--rules <path>", "watchfs.option.rules"},
			{"--exclude <glob>", "watchfs.option.exclude"},
			{"--no-default-exclude", "watchfs.option.no_default_exclude"},
			{"--debounce 200ms", "watchfs.option.debounce"},
			{"--timeout 30s (fs)", "watchfs.option.timeout"},
			{"--dry-run", "watchfs.option.dry_run"},
			{"--smart", "watchfs.option.smart"},
			{"--no-smart", "watchfs.option.no_smart"},
		},
		usesCommon: true,
		subcommands: []subcommandDoc{
			{"http", "edc watch http <host|URL>"},
			{"fs", "edc watch fs [directory]"},
		},
	},
	{
		name: "info", group: "observe",
		usage: []string{"edc info [--public=false] [--timeout 3s] [-v]"},
		options: []optionDoc{
			{"--public=false", "command.info.option.public"},
			{"--timeout 3s", "command.info.option.timeout"},
			{"-v, --verbose", "command.info.option.verbose"},
		},
	},
	{
		name: "view", group: "observe",
		usage: []string{"edc view [-n N] [-f] [file]", "<command> | edc view"},
		options: []optionDoc{
			{"-n N", "command.view.option.lines"},
			{"-f, -F", "command.view.option.follow"},
		},
		examples: []string{"edc view /var/log/nginx/access.log", "edc view -n 50000 app.log", "kubectl logs -f deploy/api | edc view", "edc view -f app.log | grep timeout"},
	},
	{
		name: "grep", group: "observe",
		usage: []string{"edc grep [options] <pattern> [path...]", "edc grep --level|--status|--since|--until ... [path...]", "<command> | edc grep [options] <pattern>"},
		options: []optionDoc{
			{"--ask", "command.grep.option.ask"},
			{"--keyword", "command.grep.option.ask_keyword"},
			{"--semantic", "command.grep.option.semantic"},
			{"--semantic-init [dir]", "command.grep.option.semantic_init"},
			{"--semantic-status [dir]", "command.grep.option.semantic_status"},
			{"--semantic-files [dir]", "command.grep.option.semantic_files"},
			{"--semantic-clear [dir]", "command.grep.option.semantic_clear"},
			{"--model-clear", "command.grep.option.model_clear"},
			{"--yes", "command.grep.option.semantic_yes"},
			{"--json", "command.grep.option.ask_json"},
			{"--limit N", "command.grep.option.ask_limit"},
			{"--code", "command.grep.option.code"},
			{"-f, --files", "command.grep.option.paths"},
			{"--hidden", "command.grep.option.hidden"},
			{"--no-ignore", "command.grep.option.no_ignore"},
			{"-i", "command.grep.option.ignore_case"},
			{"-E", "command.grep.option.regex"},
			{"-n", "command.grep.option.numbers"},
			{"-l", "command.grep.option.files"},
			{"-C N", "command.grep.option.context"},
			{"--level <level>", "command.grep.option.level"},
			{"--status <codes>", "command.grep.option.status"},
			{"--since <time>", "command.grep.option.since"},
			{"--until <time>", "command.grep.option.until"},
			{"--ordered", "command.grep.option.ordered"},
			{"--index, --no-index", "command.grep.option.index"},
			{"--jobs N", "command.grep.option.jobs"},
			{"--verify-index", "command.grep.option.verify"},
			{"--last N", "command.grep.option.last"},
		},
		examples: []string{"edc grep -f auth", "edc grep ValidateToken", "edc grep --semantic-init", "edc grep --ask 'config loader'", "edc grep --ask --keyword 'config loader'", "edc grep timeout /var/log/app/", "edc grep --since 10m --ordered --status 5xx /var/log/nginx/access.log", "edc grep --level error --last 20 app.log", "edc grep -n -C 2 'order=41' app.log*", "kubectl logs deploy/api | edc grep -i refund"},
	},
	{
		name: "du", group: "observe",
		usage: []string{"edc du [options] [path]"},
		options: []optionDoc{
			{"--top N", "command.du.option.top"},
			{"-w, --workers 4", "command.du.option.workers"},
			{"--cross-mounts", "command.du.option.cross_mounts"},
			{"-B, --no-browse", "command.du.option.no_browse"},
			{"--refresh auto", "command.du.option.refresh"},
			{"--timeout 60s", "command.du.option.timeout"},
			{"-v, --verbose", "command.du.option.verbose"},
		},
		usesCommon: true,
	},
	{
		name: "clean", group: "observe",
		usage: []string{"edc clean [options]"},
		options: []optionDoc{
			{"--progress", "command.clean.option.progress"},
			{"-w, --workers 4", "command.clean.option.workers"},
			{"--timeout 60s", "command.clean.option.timeout"},
			{"--project DIR", "command.clean.option.project"},
			{"--group NAME", "command.clean.option.group"},
			{"--inventory FILE", "command.clean.option.inventory"},
			{"-v, --verbose", "command.clean.option.verbose"},
		},
		usesCommon: true,
	},
	{
		name: "where", group: "observe",
		usage: []string{"edc where [--provider all|aws|gcp] [--count 3] [options]"},
		options: []optionDoc{
			{"--provider all", "command.where.option.provider"},
			{"--count 3", "command.where.option.count"},
		},
		usesCommon: true,
	},
	{
		name: listenProbeID, group: "observe",
		usage: []string{"edc listen [options]"},
		options: []optionDoc{
			{"--tcp", "command.listen.option.tcp"},
			{"-u, --udp", "command.listen.option.udp"},
			{"--unix", "command.listen.option.unix"},
			{"--all", "command.listen.option.all"},
			{"--watch", "command.listen.option.watch"},
			{"-i, --interval 0.1", "command.watch.option.interval"},
			{"--duration 1m", "command.watch.option.duration"},
			{"-v, --verbose", "command.listen.option.verbose"},
			{"--full", "command.listen.option.full"},
		},
		usesCommon: true,
	},
	{
		name: "svc", group: "observe",
		usage:      []string{"edc svc [options] [--all]", "edc svc [options] <unit>"},
		options:    []optionDoc{{"--all", "command.svc.option.all"}},
		usesCommon: true,
	},
	{
		name: "ctr", group: "observe",
		usage: []string{"edc ctr [options] [--all]", "edc ctr [options] <container>"},
		options: []optionDoc{
			{"--all", "command.ctr.option.all"},
			{"--collector-image <local-image>", "command.ctr.option.collector_image"},
		},
		usesCommon: true,
	},
	{
		name: "changes", group: "observe",
		usage: []string{"edc changes [--since 24h] [options]"},
		options: []optionDoc{
			{"--since 24h", "command.changes.option.since"},
			{"-v, --verbose", "command.changes.option.verbose"},
		},
		usesCommon: true,
	},
	{
		name: "quality", group: "observe",
		usage:      []string{"edc quality [options]"},
		options:    []optionDoc{{"--server <URL>", "command.quality.option.server"}},
		usesCommon: true,
	},
	{
		name: "capture", group: "observe",
		usage: []string{"edc capture --interface <name> [options]"},
		options: []optionDoc{
			{"--mode pcap|events", "command.capture.option.mode"},
			{"--interface <name>", "command.capture.option.interface"},
			{"--duration 15s", "command.capture.option.duration"},
			{"--count 500", "command.capture.option.count"},
			{"--filter <expr>", "command.capture.option.filter"},
			{"--output <path>", "command.capture.option.output"},
			{"--yes", "command.capture.option.yes"},
		},
	},
	{
		name: "trace", group: "observe",
		usage: []string{traceProtocolUsage, traceDatabaseUsage, traceSocketUsage, traceSyscallUsage, tracePrivUsage, traceReclaimUsage, traceCPUUsage, traceLockUsage, traceShowUsage},
		options: []optionDoc{
			{"--duration 15s", "command.trace.option.duration"},
			{"--slow <duration>", "command.trace.option.slow"},
			{"--json <path|->", "option.json"},
			{"--raw", "command.trace.option.raw"},
			{"--live", "command.trace.option.live"},
			{"--group-by <view>", "command.trace.option.group_by"},
			{"--process <name>", "command.trace.option.process"},
			{"--destination <host:port>", "command.trace.option.destination"},
			{"-d, --detail", "command.trace.option.detail"},
			{"--side client|server", "command.trace.option.side"},
			{"--payload[=all]", "command.trace.option.payload"},
			{"--show-secrets", "command.trace.option.show_secrets"},
			{"--tls[=<path>]", "command.trace.option.tls"},
			{"--port <n>", "command.trace.option.port"},
			{"--http-port <n>", "command.trace.option.http_port"},
			{"--container <name>", "command.trace.option.container"},
			{"--reason <names>", "command.trace.option.reason"},
			{"--device <major:minor>", "command.trace.option.device"},
			{"--signal <names>", "command.trace.option.signal"},
			{"--syscall <names>", "command.trace.option.syscall"},
			{"--errors <all|names>", "command.trace.option.errors"},
			{"--files", "command.trace.option.files"},
			{"--no-files", "command.trace.option.no_files"},
			{"--all", "command.trace.option.all"},
			{"--denied", "command.trace.option.denied"},
			{"--frequency 99", "command.trace.option.frequency"},
			{"--folded <path|->", "command.trace.option.folded"},
			{"--kind kernel|futex", "command.trace.option.lock_kind"},
			{"--off-cpu", "command.trace.option.off_cpu"},
			{"--yes", "command.trace.option.yes"},
		},
	},
	{
		name: "ai", group: "observe",
		usage: []string{
			"edc ai [--poll 60s] [--count N] [--json <path|->]",
			"edc ai statusline [-- <command> [args...]]",
			"edc ai statusline install|uninstall",
		},
		options: []optionDoc{
			{"--poll 60s", "command.ai.option.poll"},
			{"--count N", "command.ai.option.count"},
			{"--json <path|->", "option.json"},
		},
	},
	{
		name: "snapshot", group: "record",
		usage: []string{"edc snapshot [--output <file|->] [--write [db] | --no-write] [--since 6h]", "             [--force] [--du <path>]... [--timeout 15s] [--redact]"},
		options: []optionDoc{
			{"--output <file|->", "command.snapshot.option.output"},
			{"--since 6h", "command.snapshot.option.since"},
			{"--force", "command.snapshot.option.force"},
			{"--du <path>", "command.snapshot.option.du"},
			{"-w, --write [db]", "command.snapshot.option.write"},
			{"--no-write", "command.snapshot.option.no_write"},
			{"--timeout 15s", "option.timeout"},
			{"--redact", "option.redact"},
		},
	},
	{
		name: "report", group: "record",
		usage: []string{
			"edc report list [directory]",
			"edc report show <file>",
			"edc report diff [--json <path|->] <before> <after>",
		},
	},
	{
		name: "log", group: "record",
		usage: []string{
			"edc log history [options] [<command> [args...]]",
			"edc log history [options] --command <name>",
			"edc log history [options] -- <command> [args...]",
			"edc log history --key <key>",
			"edc log history --follow [--json <path|->] [options]",
			"edc log prune --older-than <age> [--dry-run] [--yes] [-v] [options]",
			"edc log [--stream stdout|stderr|both] [--output <file>]",
			"        [--command-display full|name|none] -- <command> [args...]",
		},
		options: []optionDoc{
			{"--dir <dir>", "cli.log_history.option.dir"},
			{"--file <file>", "cli.log_history.option.file"},
			{"--key <key>", "cli.log_history.option.key"},
			{"--command <name>", "cli.log_history.option.command"},
			{"--limit 20", "cli.log_history.option.limit"},
			{"--failed", "cli.log_history.option.failed"},
			{"--follow", "cli.log_history.option.follow"},
			{"--json <path|->", "cli.log_history.option.json"},
			{"--older-than 30d", "cli.log_prune.option.older_than"},
			{"--dry-run", "cli.log_prune.option.dry_run"},
			{"--yes", "cli.log_prune.option.yes"},
			{"-v", "cli.log_prune.option.verbose"},
			{"--stream stdout|stderr|both", "command.log.option.stream"},
			{"--output <file>", "command.log.option.output"},
			{"--command-display full|name|none", "command.log.option.command_display"},
			{"--max-size 10", "command.log.option.max_size"},
			{"--keep-files 3", "command.log.option.keep_files"},
			{"--restart never|on-failure|always", "command.log.option.restart"},
			{"--max-restarts 3", "command.log.option.max_restarts"},
			{"--restart-delay 5s", "command.log.option.restart_delay"},
			{"--timeout 0s", "command.log.option.timeout"},
			{"--kill-after 5s", "command.log.option.kill_after"},
		},
		subcommands: []subcommandDoc{
			{"run", "edc log -- <command> [args...]"},
			{"history", "edc log history [<command>]"},
			{"prune", "edc log prune --older-than 30d"},
		},
		examples: []string{
			"edc log -- ls -l /tmp",
			"edc log --restart on-failure -- /path/to/job",
			"edc log history",
			"edc log history --failed ls -l /tmp",
			"edc log prune --older-than 30d --dry-run",
		},
	},
	{
		name: "remote", group: "record",
		usage: []string{"edc remote [<group>] [options]"},
		options: []optionDoc{
			{"--inventory <file>", "command.remote.option.inventory"},
			{"--recipe <file>", "command.remote.option.recipe"},
			{"-n, --dry-run", "command.remote.option.dry_run"},
			{"-l, --list", "command.remote.option.list"},
			{"-f, --force", "command.remote.option.force"},
			{"--parallel N", "command.remote.option.parallel"},
			{"--group <name>", "command.remote.option.group"},
			{"--connect-timeout", "command.remote.option.connect_timeout"},
			{"--output-limit N", "command.remote.option.output_limit"},
		},
	},
	{
		name: "tunnel", group: "record",
		usage: []string{"edc tunnel", "edc tunnel list [options]", "edc tunnel up <name>", "edc tunnel up (-L [port|auto]:host:port | -R port:host:port | -D port|auto) <host>", "edc tunnel test <name>", "edc tunnel down <name>... | --all"},
		options: []optionDoc{
			{"--file <file>", "command.tunnel.option.file"},
			{"--name <name>", "command.tunnel.option.name"},
			{"-L [port|auto]:host:port", "command.tunnel.option.local"},
			{"-R port:host:port", "command.tunnel.option.reverse"},
			{"-D port|auto", "command.tunnel.option.socks"},
			{"--save", "command.tunnel.option.save"},
			{"--all", "command.tunnel.option.all"},
			{"--connect-timeout", "command.tunnel.option.connect_timeout"},
		},
		usesCommon: true,
		subcommands: []subcommandDoc{
			{"list", "edc tunnel list"},
			{"up", "edc tunnel up <name>"},
			{"test", "edc tunnel test <name>"},
			{"down", "edc tunnel down <name>"},
		},
		examples: []string{
			"edc tunnel",
			"edc tunnel up db",
			"edc tunnel up -L auto:db.internal:5432 jump1",
			"edc tunnel test db",
			"edc tunnel down --all",
		},
	},
	{
		name: "shell", group: "tool",
		usage:    []string{"edc shell [zsh|bash] [--no-alias]"},
		options:  []optionDoc{{"--no-alias", "command.shell.option.no_alias"}},
		examples: []string{"source <(edc shell)", "source <(edc shell zsh)", "source <(edc shell bash)"},
	},
	{
		name: "setup", group: "tool",
		usage: []string{"edc setup", "edc setup remote [--inventory FILE] [--recipe FILE]"},
		options: []optionDoc{
			{"--inventory FILE", "command.setup.option.inventory"},
			{"--recipe FILE", "command.setup.option.recipe"},
		},
	},
	{
		name: "update", group: "tool",
		usage: []string{"edc update [--check] [--yes] [--timeout 60s]"},
		options: []optionDoc{
			{"--check", "command.update.option.check"},
			{"--yes", "command.update.option.yes"},
			{"--timeout 60s", "command.update.option.timeout"},
		},
	},
	{
		name: "version", group: "tool",
		usage: []string{"edc version"},
	},
}

var legacyCommandDocs = []commandDoc{
	{
		name: "alias", group: "tool",
		usage: []string{"edc alias [zsh|bash] [--plain]"},
		subcommands: []subcommandDoc{
			{"zsh", "edc alias zsh"},
			{"bash", "edc alias bash"},
		},
		options: []optionDoc{
			{"--plain", "command.alias.option.plain"},
		},
	},
	{
		name: "completion", group: "tool",
		usage: []string{"edc completion <zsh|bash|groups>"},
		subcommands: []subcommandDoc{
			{"zsh", "edc completion zsh"},
			{"bash", "edc completion bash"},
			{"groups", "edc completion groups"},
		},
	},
}

func findCommandDoc(name string) (commandDoc, bool) {
	for _, doc := range commandDocs {
		if doc.name == name {
			return doc, true
		}
	}
	for _, doc := range legacyCommandDocs {
		if doc.name == name {
			return doc, true
		}
	}
	return commandDoc{}, false
}

// commandNames는 completion script가 쓰는 명령 이름 목록이다.
func commandNames() []string {
	names := make([]string, 0, len(commandDocs))
	for _, doc := range commandDocs {
		names = append(names, doc.name)
	}
	sort.Strings(names)
	return names
}

// commandSummaries는 zsh completion이 쓰는 `이름:설명` 목록이다.
func commandSummaries() []commandDoc {
	docs := append([]commandDoc(nil), commandDocs...)
	sort.Slice(docs, func(i, j int) bool { return docs[i].name < docs[j].name })
	return docs
}

// printHelp는 첫 화면이다. 명령마다 한 줄만 두고 상세는 edc help <command>로 미룬다.
func printHelp(writer io.Writer, version string) {
	if helpColor(writer) {
		printRichHelp(writer, version)
		return
	}
	fmt.Fprintf(writer, "edc %s — %s\n\n", version, T("help.tagline"))

	labels, labelWidth := helpLabels()
	fmt.Fprintf(writer, "%s  %s\n", padRight(labels[0], labelWidth), T("help.usage"))

	for _, group := range helpGroups {
		members, width := helpGroupMembers(group)
		if len(members) == 0 {
			continue
		}
		fmt.Fprintf(writer, "\n%s\n", T("help.group."+group))
		for _, doc := range members {
			fmt.Fprintf(writer, "  %-*s  %s\n", width, doc.name, doc.summary())
		}
	}

	flags := make([]string, 0, len(commonOptionDocs))
	for _, option := range commonOptionDocs {
		flags = append(flags, option.flag)
	}
	fmt.Fprintf(writer, "\n%s  %s\n", padRight(labels[1], labelWidth), strings.Join(flags, "  "))
	fmt.Fprintf(writer, "%s  %s\n", padRight(labels[2], labelWidth), T("help.detail_hint"))
}

// helpLabels는 첫 화면 왼쪽의 세 라벨과 그 폭이다. 라벨 폭은 언어마다 다르므로 세 라벨을 재서 맞춘다.
func helpLabels() ([]string, int) {
	labels := []string{T("help.usage_label"), T("help.common_label"), T("help.detail_label")}
	labelWidth := 0
	for _, label := range labels {
		if width := liveWidth(label); width > labelWidth {
			labelWidth = width
		}
	}
	return labels, labelWidth
}

func helpGroupMembers(group string) ([]commandDoc, int) {
	var members []commandDoc
	width := 0
	for _, doc := range commandDocs {
		if doc.group != group {
			continue
		}
		members = append(members, doc)
		if len(doc.name) > width {
			width = len(doc.name)
		}
	}
	return members, width
}

// printRichHelp는 색을 쓰는 터미널의 첫 화면이다. 이름 폭은 평문처럼 그룹마다 잰다.
// 모든 그룹을 가장 긴 이름(completion)에 맞추면 ko와 ja의 긴 summary가 80칸을 넘는다.
func printRichHelp(writer io.Writer, version string) {
	fmt.Fprintf(writer, "%s %s  %s\n\n", richBadge("edc", richMagenta), richDim(version), richBold(T("help.tagline")))

	labels, labelWidth := helpLabels()
	fmt.Fprintf(writer, "%s  %s\n", richDim(padRight(labels[0], labelWidth)), T("help.usage"))

	for _, group := range helpGroups {
		members, width := helpGroupMembers(group)
		if len(members) == 0 {
			continue
		}
		fmt.Fprintf(writer, "\n%s\n", richPanelTop(T("help.group."+group), strconv.Itoa(len(members)), ""))
		for _, doc := range members {
			fmt.Fprintln(writer, richPanelLine(richPaint(padRight(doc.name, width), richCyan, true)+"  "+doc.summary(), ""))
		}
		fmt.Fprintln(writer, richPanelBottom(""))
	}

	chips := make([]string, 0, len(commonOptionDocs))
	for _, option := range commonOptionDocs {
		chips = append(chips, richChip(option.flag))
	}
	fmt.Fprintf(writer, "\n%s  %s\n", richDim(padRight(labels[1], labelWidth)), strings.Join(chips, " "))
	fmt.Fprintf(writer, "%s  %s\n", richDim(padRight(labels[2], labelWidth)), richPaint(T("help.detail_hint"), richCyan, true))
}

// printCommandHelp는 명령 하나의 상세다. 그 명령에 관한 설명을 모두 여기 모은다.
func printCommandHelp(writer io.Writer, name string) bool {
	doc, ok := findCommandDoc(name)
	if !ok {
		return false
	}
	if helpColor(writer) {
		printRichCommandHelp(writer, doc)
		return true
	}
	renderCommandHelp(writer, doc, 80, false)
	return true
}

// printCommandBrief는 하위 명령을 빼고 실행했을 때의 짧은 안내다. 하위 명령마다 무엇을 하는지와
// 바로 칠 수 있는 예시만 보이고, option 설명은 edc help <command>로 미룬다.
func printCommandBrief(writer io.Writer, name string) bool {
	doc, ok := findCommandDoc(name)
	if !ok || len(doc.subcommands) == 0 {
		return false
	}
	if helpColor(writer) {
		printRichCommandBrief(writer, doc)
		return true
	}
	fmt.Fprintf(writer, "edc %s — %s\n\n%s\n", doc.name, doc.summary(), T("help.usage_label"))
	printOptionDocs(writer, doc.subcommandLines())
	printExamples(writer, doc.examples)
	fmt.Fprintf(writer, "\n%s  edc help %s\n", T("help.detail_label"), doc.name)
	return true
}

// printRichCommandBrief는 안내 화면을 색 help와 같은 배지와 패널로 그린다. 형태와 예시는 그대로 칠 수 있는
// 명령이라 굵은 청록이다.
func printRichCommandBrief(writer io.Writer, doc commandDoc) {
	fmt.Fprintf(writer, "%s %s\n\n", richBadge("edc "+doc.name, richMagenta), richBold(doc.summary()))
	fmt.Fprintln(writer, richPanelTop(T("help.usage_label"), "", ""))
	printRichOptionDocs(writer, doc.subcommandLines())
	fmt.Fprintln(writer, richPanelBottom(""))
	printRichExamples(writer, doc.examples)
	fmt.Fprintf(writer, "\n%s  %s\n", richDim(T("help.detail_label")), richPaint("edc help "+doc.name, richCyan, true))
}

func printRichExamples(writer io.Writer, examples []string) {
	if len(examples) == 0 {
		return
	}
	render := helpRenderer{output: writer, width: commandHelpWidth(), color: true}
	render.panel(T("help.examples_label"), func() {
		for _, example := range examples {
			render.syntax(example)
		}
	})
}

func printExamples(writer io.Writer, examples []string) {
	if len(examples) == 0 {
		return
	}
	fmt.Fprintf(writer, "\n%s\n", T("help.examples_label"))
	for _, example := range examples {
		fmt.Fprintf(writer, "  %s\n", example)
	}
}

// subcommandSelectItems는 하위 명령을 고르는 화면의 항목이다. 이름만으로는 무엇을 하는지 알 수
// 없으므로 help와 같은 설명을 옆에 붙인다. 설명이 없는 하위 명령은 이름만 보인다.
func subcommandSelectItems(name string, choices []string) []selectItem {
	doc, _ := findCommandDoc(name)
	described := make(map[string]bool, len(doc.subcommands))
	for _, sub := range doc.subcommands {
		described[sub.name] = true
	}
	width := 0
	for _, choice := range choices {
		width = max(width, liveWidth(choice))
	}
	items := make([]selectItem, 0, len(choices))
	for _, choice := range choices {
		label := choice
		if described[choice] {
			label = padRight(choice, width) + "  — " + T(doc.subcommandKey(choice))
		}
		items = append(items, selectItem{label: label, value: choice})
	}
	return items
}

// printRichCommandHelp는 평문과 같은 순서로 usage, options, 공용 options를 회색 패널에 담는다.
// 배지 뒤는 한 칸만 띄운다. 배지가 평문의 "edc name — "과 같은 폭이 되어 긴 summary도 80칸 안에 든다.
func printRichCommandHelp(writer io.Writer, doc commandDoc) {
	renderCommandHelp(writer, doc, commandHelpWidth(), true)
}

// uncoveredCommonOptions는 명령이 직접 설명한 공용 flag를 공용 목록에서 뺀다. 같은 flag가 뜻이 다른
// 두 줄로 나오면 어느 쪽이 맞는지 알 수 없다. 명령이 적은 설명이 이긴다.
func uncoveredCommonOptions(own []optionDoc) []optionDoc {
	covered := make(map[string]bool, len(own))
	for _, option := range own {
		covered[option.flag] = true
	}
	remaining := make([]optionDoc, 0, len(commonOptionDocs))
	for _, option := range commonOptionDocs {
		if !covered[option.flag] {
			remaining = append(remaining, option)
		}
	}
	return remaining
}

func printOptionDocs(writer io.Writer, options []optionDoc) {
	width := optionFlagWidth(options)
	for _, option := range options {
		detail := option.detail()
		if detail == "" {
			fmt.Fprintf(writer, "  %s\n", option.flag)
			continue
		}
		available := max(1, commandHelpWidth()-width-4)
		fmt.Fprintf(writer, "  %s  %s\n", padRight(option.flag, width), ansi.Truncate(detail, available, "…"))
	}
}

// printRichOptionDocs는 printOptionDocs와 같은 열을 패널 줄로 그린다. flag만 굵은 청록이다.
func printRichOptionDocs(writer io.Writer, options []optionDoc) {
	(helpRenderer{output: writer, width: commandHelpWidth(), color: true}).options(options)
}

func optionFlagWidth(options []optionDoc) int {
	width := 0
	for _, option := range options {
		if value := liveWidth(option.flag); value > width {
			width = value
		}
	}
	return width
}

// helpColor는 help를 받는 writer가 색을 쓰는 터미널인지다. 파이프와 test의 strings.Builder는 평문을 받는다.
func helpColor(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	return ok && richEnabled(file)
}
