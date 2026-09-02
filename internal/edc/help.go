package edc

import (
	"fmt"
	"io"
	"sort"
	"strings"
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

// commandDoc은 명령 하나의 뼈대다. 이름과 문법은 코드에, 설명은 locale에 둔다.
type commandDoc struct {
	name       string
	group      string
	usage      []string
	options    []optionDoc
	usesCommon bool
}

func (doc commandDoc) summary() string { return T("command." + doc.name + ".summary") }

func (doc commandDoc) notes() []string { return TList("command." + doc.name + ".notes") }

// helpGroups는 첫 화면의 그룹 순서다. 값은 locale의 help.group 아래 키다.
var helpGroups = []string{"diagnose", "observe", "record", "tool"}

var commonOptionDocs = []optionDoc{
	{"--timeout 15s", "option.timeout"},
	{"--json <path|->", "option.json"},
	{"-v, --verbose", "option.verbose"},
	{"--redact=true", "option.redact"},
}

// commandDocs는 help와 completion이 함께 읽는 한 벌의 명령 목록이다.
// 설명을 여기 한 곳에만 두어 두 출력이 어긋나지 않게 한다.
var commandDocs = []commandDoc{
	{
		name: "doctor", group: "diagnose",
		usage:      []string{"edc doctor [--profile default|full] [options] <host|URL>"},
		options:    []optionDoc{{"--profile default|full", "command.doctor.option.profile"}},
		usesCommon: true,
	},
	{
		name: "dns", group: "diagnose",
		usage:      []string{"edc dns lookup [options] <host>", "edc dns config [options]"},
		usesCommon: true,
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
		usage:      []string{"edc http check [--expect-status N] [options] <URL>"},
		options:    []optionDoc{{"--expect-status N", "command.http.option.expect_status"}},
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
	},
	{
		name: "top", group: "observe",
		usage: []string{"edc top [--interval 1s] [--count N] [--json <path|->]"},
		options: []optionDoc{
			{"--interval 1s", "command.top.option.interval"},
			{"--count N", "command.top.option.count"},
			{"--no-header", "command.top.option.no_header"},
			{"--json <path|->", "command.top.option.json"},
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
		name: "where", group: "observe",
		usage: []string{"edc where [--provider all|aws|gcp] [--count 3] [options]"},
		options: []optionDoc{
			{"--provider all", "command.where.option.provider"},
			{"--count 3", "command.where.option.count"},
		},
		usesCommon: true,
	},
	{
		name: "sockets", group: "observe",
		usage: []string{"edc sockets [options]"}, usesCommon: true,
	},
	{
		name: "quality", group: "observe",
		usage: []string{"edc quality [options]"}, usesCommon: true,
	},
	{
		name: "capture", group: "observe",
		usage: []string{"edc capture --interface <name> [options]"},
		options: []optionDoc{
			{"--interface <name>", "command.capture.option.interface"},
			{"--duration 15s", "command.capture.option.duration"},
			{"--count 500", "command.capture.option.count"},
			{"--filter <expr>", "command.capture.option.filter"},
			{"--output <path>", "command.capture.option.output"},
			{"--yes", "command.capture.option.yes"},
		},
	},
	{
		name: "report", group: "record",
		usage: []string{
			"edc report show <file>",
			"edc report diff [--json <path|->] <before> <after>",
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
		},
	},
	{
		name: "completion", group: "tool",
		usage: []string{"edc completion <zsh|bash|groups>"},
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

func findCommandDoc(name string) (commandDoc, bool) {
	for _, doc := range commandDocs {
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
func printHelp(writer io.Writer) {
	fmt.Fprintf(writer, "edc — %s\n\n", T("help.tagline"))

	// 라벨 폭은 언어마다 다르므로 세 라벨을 재서 맞춘다.
	labels := []string{T("help.usage_label"), T("help.common_label"), T("help.detail_label")}
	labelWidth := 0
	for _, label := range labels {
		if width := liveWidth(label); width > labelWidth {
			labelWidth = width
		}
	}
	fmt.Fprintf(writer, "%s  %s\n", padRight(labels[0], labelWidth), T("help.usage"))

	for _, group := range helpGroups {
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

// printCommandHelp는 명령 하나의 상세다. 그 명령에 관한 설명을 모두 여기 모은다.
func printCommandHelp(writer io.Writer, name string) bool {
	doc, ok := findCommandDoc(name)
	if !ok {
		return false
	}
	fmt.Fprintf(writer, "edc %s — %s\n\n%s\n", doc.name, doc.summary(), T("help.usage_label"))
	for _, line := range doc.usage {
		fmt.Fprintf(writer, "  %s\n", line)
	}
	if len(doc.options) > 0 {
		fmt.Fprintf(writer, "\n%s\n", T("help.options_label"))
		printOptionDocs(writer, doc.options)
	}
	if doc.usesCommon {
		fmt.Fprintf(writer, "\n%s\n", T("help.common_options_label"))
		printOptionDocs(writer, commonOptionDocs)
	}
	if notes := doc.notes(); len(notes) > 0 {
		fmt.Fprintln(writer)
		for _, note := range notes {
			fmt.Fprintf(writer, "%s\n", note)
		}
	}
	return true
}

func printOptionDocs(writer io.Writer, options []optionDoc) {
	width := 0
	for _, option := range options {
		if value := liveWidth(option.flag); value > width {
			width = value
		}
	}
	for _, option := range options {
		detail := option.detail()
		if detail == "" {
			fmt.Fprintf(writer, "  %s\n", option.flag)
			continue
		}
		fmt.Fprintf(writer, "  %s  %s\n", padRight(option.flag, width), detail)
	}
}
