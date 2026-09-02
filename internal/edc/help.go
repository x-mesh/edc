package edc

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// optionDoc은 상세 화면에 그리는 option 한 줄이다.
type optionDoc struct {
	flag   string
	detail string
}

// commandDoc은 명령 하나의 문서다. summary는 첫 화면에, 나머지는 `edc help <command>`에 쓴다.
type commandDoc struct {
	name       string
	group      string
	summary    string
	usage      []string
	options    []optionDoc
	usesCommon bool
	notes      []string
}

// helpGroups는 첫 화면의 그룹 순서다.
var helpGroups = []string{"진단", "관측", "기록·원격", "도구"}

var commonOptionDocs = []optionDoc{
	{"--timeout 15s", "실행 제한 시간"},
	{"--json <path|->", "JSON 출력 경로, stdout은 -"},
	{"-v, --verbose", "상세 evidence 출력"},
	{"--redact=true", "JSON 민감정보 redaction"},
}

// commandDocs는 help와 completion이 함께 읽는 한 벌의 명령 목록이다.
// 설명을 여기 한 곳에만 두어 두 출력이 어긋나지 않게 한다.
var commandDocs = []commandDoc{
	{
		name: "doctor", group: "진단", summary: "DNS·TCP·TLS·HTTP를 한 번에 확인",
		usage:      []string{"edc doctor [--profile default|full] [options] <host|URL>"},
		options:    []optionDoc{{"--profile default|full", "full은 bandwidth와 responsiveness를 더합니다"}},
		usesCommon: true,
		notes: []string{
			"probe 9개를 한 번에 실행하고 결과를 같은 형식으로 모읍니다.",
			"terminal에서는 probe마다 한 줄씩 실시간으로 갱신합니다.",
			"Ctrl-C로 취소하면 exit code 4를 돌려줍니다.",
		},
	},
	{
		name: "dns", group: "진단", summary: "이름 조회와 resolver 설정",
		usage:      []string{"edc dns lookup [options] <host>", "edc dns config [options]"},
		usesCommon: true,
		notes:      []string{"lookup은 주소를, config는 이 host의 resolver 설정을 봅니다."},
	},
	{
		name: "tcp", group: "진단", summary: "TCP 연결 확인",
		usage:      []string{"edc tcp check [options] <host:port>"},
		usesCommon: true,
		notes:      []string{"연결하지 못하면 phase와 cause를 남기고 exit code 1을 돌려줍니다."},
	},
	{
		name: "tls", group: "진단", summary: "인증서와 handshake 확인",
		usage:      []string{"edc tls check [--min-days N] [options] <host:port>"},
		options:    []optionDoc{{"--min-days N", "남은 일수가 N보다 작으면 fail로 처리합니다"}},
		usesCommon: true,
		notes:      []string{"기본값은 30일 미만일 때 warning입니다. --min-days는 그 기준을 fail로 올립니다."},
	},
	{
		name: "http", group: "진단", summary: "HTTP 응답 확인",
		usage:      []string{"edc http check [--expect-status N] [options] <URL>"},
		options:    []optionDoc{{"--expect-status N", "응답 code가 N과 다르면 fail로 처리합니다"}},
		usesCommon: true,
		notes:      []string{"기본값은 4xx가 warning, 5xx가 fail입니다."},
	},
	{
		name: "net", group: "진단", summary: "interface, route, ping, trace",
		usage: []string{
			"edc net interfaces [options]",
			"edc net route [options] <host>",
			"edc net ping [options] <host>",
			"edc net trace [options] <host>",
		},
		usesCommon: true,
	},
	{
		name: "top", group: "관측", summary: "실시간 resource 대시보드",
		usage: []string{"edc top [--interval 1s] [--count N] [--json <path|->]"},
		options: []optionDoc{
			{"--interval 1s", "sampling 간격"},
			{"--count N", "출력할 row 수, 0은 계속 실행"},
			{"--no-header", "host와 column header 생략"},
			{"--json <path|->", "sample당 한 줄 JSON, stdout은 -"},
		},
		notes: []string{
			"terminal에서는 대시보드로 엽니다. q 종료, p 일시정지, +/- interval.",
			"--count나 --json을 주거나, 비 terminal이거나, NO_COLOR면 표로 출력합니다.",
		},
	},
	{
		name: "info", group: "관측", summary: "system, network, disk 정보",
		usage: []string{"edc info [--public=false] [--timeout 3s] [-v]"},
		options: []optionDoc{
			{"--public=false", "외부 ipinfo.io 조회를 끕니다"},
			{"--timeout 3s", "public 조회 제한 시간"},
			{"-v, --verbose", "public 조회 실패 원인 출력"},
		},
		notes: []string{
			"public IP를 기본으로 조회하고, 제한 시간 안에 응답이 없으면 그 줄을 뺍니다.",
			"disk 사용량은 전체 크기에서 남은 공간을 뺀 값입니다.",
		},
	},
	{
		name: "sockets", group: "관측", summary: "열린 socket 목록",
		usage: []string{"edc sockets [options]"}, usesCommon: true,
		notes: []string{"-v를 주면 목록 전체를 상세로 펼칩니다."},
	},
	{
		name: "quality", group: "관측", summary: "networkQuality 측정 (macOS)",
		usage: []string{"edc quality [options]"}, usesCommon: true,
		notes: []string{"macOS의 networkQuality를 씁니다. 측정에 수십 초가 걸립니다."},
	},
	{
		name: "capture", group: "관측", summary: "packet capture (macOS)",
		usage: []string{"edc capture --interface <name> [options]"},
		options: []optionDoc{
			{"--interface <name>", "capture할 interface (필수)"},
			{"--duration 15s", "capture 시간, 최대 60s"},
			{"--count 500", "packet 수, 최대 10000"},
			{"--filter <expr>", "BPF filter"},
			{"--output <path>", "pcap 저장 경로"},
			{"--yes", "확인 질문 생략"},
		},
		notes: []string{
			"privileged 작업입니다. 시작 전에 계획을 보여 주고 확인을 받습니다.",
			"PCAP에는 credential과 개인정보가 들어갈 수 있습니다.",
			"JSON redaction은 PCAP payload에 적용되지 않습니다.",
		},
	},
	{
		name: "report", group: "기록·원격", summary: "저장한 JSON report 보기와 비교",
		usage: []string{
			"edc report show <file>",
			"edc report diff [--json <path|->] <before> <after>",
		},
		notes: []string{
			"terminal에서는 뷰어로 엽니다. f 필터, e 상세, q 종료.",
			"diff는 probe별 status 변화와 metric 차이를 보여 줍니다.",
			"악화된 probe가 있으면 exit code 1을 돌려줍니다.",
		},
	},
	{
		name: "remote", group: "기록·원격", summary: "inventory group에 recipe 실행",
		usage: []string{"edc remote [<group>] [options]"},
		options: []optionDoc{
			{"--inventory <file>", "inventory YAML 경로"},
			{"--recipe <file>", "recipe YAML 경로"},
			{"-n, --dry-run", "계획만 출력하고 종료"},
			{"-l, --list", "inventory의 group과 host 출력"},
			{"-f, --force", "확인 질문 생략"},
			{"--parallel N", "동시에 실행할 host 수"},
		},
		notes: []string{
			"group을 생략하면 선택기를 띄웁니다.",
			"inventory.yaml과 recipe.yaml은 현재 디렉터리와 config 디렉터리에서 찾습니다.",
			"계획과 결과가 host×step 표 하나를 씁니다.",
			"Ctrl-C로 취소하면 남은 step을 SKIP으로 표시하고 exit code 4를 돌려줍니다.",
		},
	},
	{
		name: "completion", group: "도구", summary: "shell completion 출력",
		usage: []string{"edc completion <zsh|bash|groups>"},
		notes: []string{
			"source <(edc completion zsh) 또는 source <(edc completion bash).",
			"groups는 inventory의 group 이름을 한 줄에 하나씩 출력합니다.",
		},
	},
	{
		name: "update", group: "도구", summary: "최신 release 설치",
		usage: []string{"edc update [--check] [--yes] [--timeout 60s]"},
		options: []optionDoc{
			{"--check", "새 버전만 확인하고 설치하지 않습니다"},
			{"--yes", "확인 질문 생략"},
			{"--timeout 60s", "network 제한 시간"},
		},
		notes: []string{
			"GitHub release에서 asset을 받아 SHA-256을 확인한 다음 실행 파일을 바꿉니다.",
			"새 파일을 옆에 쓰고 이름을 바꾸므로 실패해도 기존 파일이 남습니다.",
			"설치 디렉터리에 쓸 권한이 없으면 내려받기 전에 exit code 3으로 멈춥니다.",
		},
	},
	{
		name: "version", group: "도구", summary: "버전 출력",
		usage: []string{"edc version"},
		notes: []string{"terminal에서는 banner로, 그 밖에서는 한 줄로 출력합니다."},
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
	fmt.Fprint(writer, "edc — everyday carry for SE/SRE\n\n")
	fmt.Fprint(writer, "사용법  edc <command> [options] [target]\n")

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
		fmt.Fprintf(writer, "\n%s\n", group)
		for _, doc := range members {
			fmt.Fprintf(writer, "  %-*s  %s\n", width, doc.name, doc.summary)
		}
	}

	flags := make([]string, 0, len(commonOptionDocs))
	for _, option := range commonOptionDocs {
		flags = append(flags, option.flag)
	}
	fmt.Fprintf(writer, "\n공통    %s\n", strings.Join(flags, "  "))
	fmt.Fprint(writer, "자세히  edc help <command>\n")
}

// printCommandHelp는 명령 하나의 상세다. 그 명령에 관한 설명을 모두 여기 모은다.
func printCommandHelp(writer io.Writer, name string) bool {
	doc, ok := findCommandDoc(name)
	if !ok {
		return false
	}
	fmt.Fprintf(writer, "edc %s — %s\n\n사용법\n", doc.name, doc.summary)
	for _, line := range doc.usage {
		fmt.Fprintf(writer, "  %s\n", line)
	}
	if len(doc.options) > 0 {
		fmt.Fprint(writer, "\noptions\n")
		printOptionDocs(writer, doc.options)
	}
	if doc.usesCommon {
		fmt.Fprint(writer, "\n공통 options\n")
		printOptionDocs(writer, commonOptionDocs)
	}
	if len(doc.notes) > 0 {
		fmt.Fprintln(writer)
		for _, note := range doc.notes {
			fmt.Fprintf(writer, "%s\n", note)
		}
	}
	return true
}

func printOptionDocs(writer io.Writer, options []optionDoc) {
	width := 0
	for _, option := range options {
		if len(option.flag) > width {
			width = len(option.flag)
		}
	}
	for _, option := range options {
		if option.detail == "" {
			fmt.Fprintf(writer, "  %s\n", option.flag)
			continue
		}
		fmt.Fprintf(writer, "  %-*s  %s\n", width, option.flag, option.detail)
	}
}
