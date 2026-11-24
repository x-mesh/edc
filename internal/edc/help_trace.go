package edc

import (
	"fmt"
	"io"
	"strings"
)

// traceNoteGroups는 edc help trace가 notes를 잇는 순서다. 묶음은 locale의 command.trace.note_group 아래에 있다.
var traceNoteGroups = []string{
	"screen", "grouped", "stop", "raw", "slow", "container", "flow", "side", "dns", "arp", "ndp", "http",
	"mysql", "clickhouse", "postgres", "redis", "socket", "drop", "io", "sched", "exec", "syscall", "priv",
	"reclaim", "cpu", "lock", "show",
}

// traceSubDoc는 edc help trace <하위 명령>의 뼈대다. options는 trace 도움말의 option 중 이 하위 명령의 parser가 받는 것이다.
// --live는 받기만 하고 효과가 없어서 넣지 않는다.
type traceSubDoc struct {
	name       string
	usage      string
	options    []string
	noteGroups []string
	examples   []string
}

// 공용 trace 화면을 여는 하위 명령이 함께 받는 option과 notes다.
var (
	traceScreenOptions = []string{"duration", "json", "raw", "group-by", "process", "destination", "detail"}
	traceScreenNotes   = []string{"screen", "grouped", "stop", "raw"}
)

func traceScreenSub(name string, options, notes, examples []string) traceSubDoc {
	return traceSubDoc{
		name: name, usage: "edc trace " + name + " [options]",
		options:    append(append([]string(nil), traceScreenOptions...), options...),
		noteGroups: append(append([]string(nil), traceScreenNotes...), notes...),
		examples:   examples,
	}
}

// traceSubDocs의 option은 runTraceProtocol의 검사와 하위 명령마다 따로 둔 parser를 따른다. TestTraceSubHelpOptionsParse가
// 각 parser가 이 option을 받는지 확인한다.
var traceSubDocs = []traceSubDoc{
	traceScreenSub("tcp", []string{"container"}, []string{"container", "flow"},
		[]string{"edc trace tcp --duration 30s --group-by target", "edc trace tcp --duration 30s --destination 192.0.2.10:443 -d", "edc trace tcp --duration 30s --raw > tcp.jsonl"}),
	traceScreenSub("udp", []string{"container"}, []string{"container", "flow"},
		[]string{"edc trace udp --duration 15s", "edc trace udp --group-by target"}),
	traceScreenSub("dns", []string{"side", "container"}, []string{"container", "side", "dns"},
		[]string{"edc trace dns", "edc trace dns --group-by target", "edc trace dns --process curl --destination 127.0.0.53:53"}),
	traceScreenSub("arp", nil, []string{"arp"}, []string{"edc trace arp", "edc trace arp --group-by target"}),
	traceScreenSub("ndp", nil, []string{"arp", "ndp"}, []string{"edc trace ndp", "edc trace ndp --group-by target"}),
	traceScreenSub("http", []string{"yes", "side", "payload", "show-secrets", "stats-only", "tls", "tls-keylog", "port", "container"}, []string{"container", "side", "http"},
		[]string{"edc trace http", "edc trace http --side server --port 8080", "edc trace http --group-by path", "edc trace http --stats-only --port 8080", "edc trace http --side server --container web"}),
	traceScreenSub("mysql", []string{"slow", "side", "show-secrets", "tls", "port", "container"}, []string{"slow", "container", "side", "mysql"},
		[]string{"edc trace mysql", "edc trace mysql --side server --port 3307", "edc trace mysql --slow 250ms"}),
	traceScreenSub("clickhouse", []string{"slow", "side", "show-secrets", "port", "http-port", "container"}, []string{"slow", "container", "side", "clickhouse"},
		[]string{"edc trace clickhouse", "edc trace clickhouse --port 9001 --http-port 18123", "edc trace clickhouse --slow 500ms"}),
	traceScreenSub("postgres", []string{"slow", "side", "show-secrets", "port", "container"}, []string{"slow", "container", "side", "postgres"},
		[]string{"edc trace postgres", "edc trace postgres --port 6432", "edc trace postgres --slow 250ms"}),
	traceScreenSub("redis", []string{"slow", "side", "show-secrets", "tls", "port", "container"}, []string{"slow", "container", "side", "redis"},
		[]string{"edc trace redis", "edc trace redis --port 6380", "edc trace redis --slow 5ms"}),
	{
		name: "socket", usage: traceSocketUsage,
		options:    append(append([]string(nil), traceScreenOptions...), "payload", "container"),
		noteGroups: append(append([]string(nil), traceScreenNotes...), "container", "socket"),
		examples:   []string{"edc trace socket /run/docker.sock", "edc trace socket /run/docker.sock --payload", "edc trace socket /run/php/php-fpm.sock --payload=all --raw"},
	},
	traceScreenSub("drop", []string{"reason", "container"}, []string{"container", "drop"},
		[]string{"edc trace drop", "edc trace drop --reason NO_SOCKET,SOCKET_RCVBUFF", "edc trace drop --container web --raw"}),
	{
		name: "io", usage: "edc trace io [options]",
		options:    []string{"duration", "slow", "process", "device", "group-by", "raw", "json", "detail", "container"},
		noteGroups: []string{"stop", "raw", "slow", "container", "io"},
		examples:   []string{"edc trace io --duration 15s", "edc trace io --device 8:0 --group-by device", "edc trace io --process postgres --raw"},
	},
	{
		name: "sched", usage: "edc trace sched [options]",
		options:    []string{"duration", "slow", "process", "container", "group-by", "raw", "json", "detail"},
		noteGroups: []string{"stop", "raw", "slow", "container", "sched"},
		examples:   []string{"edc trace sched --duration 15s", "edc trace sched --process api --group-by process", "edc trace sched --raw"},
	},
	{
		name: "exec", usage: "edc trace exec [options]",
		options:    []string{"duration", "process", "container", "group-by", "signal", "raw", "json", "detail"},
		noteGroups: []string{"stop", "raw", "container", "exec"},
		examples:   []string{"edc trace exec --duration 15s", "edc trace exec --process cron,sh --group-by parent", "edc trace exec --signal all --raw"},
	},
	{
		name: "syscall", usage: traceSyscallUsage,
		options:    []string{"duration", "process", "container", "group-by", "json", "syscall", "slow", "errors", "raw", "all", "files", "no-files", "fd-path"},
		noteGroups: []string{"stop", "raw", "slow", "container", "syscall"},
		examples:   []string{"edc trace syscall --duration 10s", "edc trace syscall -- curl -s https://example.com", "edc trace syscall --process nginx --group-by pid,syscall --duration 30s"},
	},
	{
		name: "priv", usage: tracePrivUsage,
		options:    []string{"duration", "process", "container", "json", "raw", "denied"},
		noteGroups: []string{"stop", "raw", "container", "priv"},
		examples:   []string{"edc trace priv --duration 30s", "edc trace priv --denied --container web"},
	},
	{
		name: "reclaim", usage: traceReclaimUsage,
		options:    []string{"duration", "process", "container", "group-by", "json", "slow", "raw"},
		noteGroups: []string{"stop", "raw", "slow", "container", "reclaim"},
		examples:   []string{"edc trace reclaim --duration 30s", "edc trace reclaim --container web --slow 10ms"},
	},
	{
		name: "cpu", usage: traceCPUUsage,
		options:    []string{"duration", "frequency", "process", "container", "json", "folded", "off-cpu"},
		noteGroups: []string{"stop", "container", "cpu"},
		examples:   []string{"edc trace cpu --duration 30s", "edc trace cpu --off-cpu --process api --duration 10s", "edc trace cpu --frequency 499 --folded - | flamegraph.pl > cpu.svg"},
	},
	{
		name: "lock", usage: traceLockUsage,
		options:    []string{"duration", "process", "container", "json", "kind"},
		noteGroups: []string{"stop", "container", "lock"},
		examples:   []string{"edc trace lock --duration 10s", "edc trace lock --process api --kind futex"},
	},
	{
		name: "show", usage: traceShowUsage,
		options:    []string{"group-by", "process", "destination", "json", "detail"},
		noteGroups: []string{"show"},
		examples:   []string{"edc trace show http.jsonl", "edc trace show http.jsonl --group-by path -d", "edc trace show tcp.jsonl dns.jsonl http.jsonl drop.jsonl"},
	},
}

// traceOptionDoc은 trace 도움말에서 이름이 name인 option 줄이다.
func traceOptionDoc(parent commandDoc, name string) (optionDoc, bool) {
	for _, option := range parent.options {
		for _, word := range strings.FieldsFunc(option.flag, func(r rune) bool { return r == ' ' || r == ',' || r == '[' || r == '=' }) {
			if word == "--"+name {
				return option, true
			}
		}
	}
	return optionDoc{}, false
}

// traceSubCommandDoc은 하위 명령 하나의 도움말이다. 기존 도움말 renderer가 그리도록 commandDoc으로 만든다.
func traceSubCommandDoc(name string) (commandDoc, bool) {
	parent, ok := findCommandDoc("trace")
	if !ok {
		return commandDoc{}, false
	}
	for _, sub := range traceSubDocs {
		if sub.name != name {
			continue
		}
		doc := commandDoc{name: "trace " + sub.name, group: parent.group, usage: []string{sub.usage}, examples: sub.examples, parent: "trace", noteGroups: sub.noteGroups}
		if name == "tcp" {
			doc.summaryKey = "command.trace.tcp_summary"
		}
		for _, option := range sub.options {
			if line, ok := traceOptionDoc(parent, option); ok {
				if name == "cpu" && option == "yes" {
					line.key = "command.trace.option.cpu_yes"
				}
				if name == "tcp" && (option == "group-by" || option == "destination") {
					line.key = "command.trace.option.tcp_" + strings.ReplaceAll(option, "-", "_")
				}
				doc.options = append(doc.options, line)
			}
		}
		return doc, true
	}
	return commandDoc{}, false
}

// traceSubNames는 하위 명령 도움말이 있는 이름이다.
func traceSubNames() []string {
	names := make([]string, 0, len(traceSubDocs))
	for _, sub := range traceSubDocs {
		names = append(names, sub.name)
	}
	return names
}

// printTraceSubHelp는 edc help trace <하위 명령>이다. 모르는 하위 명령이면 아무것도 쓰지 않고 false다.
func printTraceSubHelp(writer io.Writer, name string) bool {
	doc, ok := traceSubCommandDoc(name)
	if !ok {
		return false
	}
	color := helpColor(writer)
	width := 80
	if color {
		width = commandHelpWidth()
	}
	renderCommandHelp(writer, doc, width, color)
	printHelpDetail(writer, "edc help trace", color)
	return true
}

// printHelpDetail은 도움말 끝에 더 볼 명령을 한 줄로 적는다.
func printHelpDetail(writer io.Writer, command string, color bool) {
	if color {
		fmt.Fprintf(writer, "\n%s  %s\n", richDim(T("help.detail_label")), richPaint(command, richCyan, true))
		return
	}
	fmt.Fprintf(writer, "\n%s  %s\n", T("help.detail_label"), command)
}
