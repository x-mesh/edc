package edc

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

type commonOptions struct {
	jsonPath string
	timeout  time.Duration
	verbose  bool
	redact   bool
}

func Run(args []string, version string) int {
	initLanguage()
	if len(args) == 0 {
		printHelp(os.Stdout)
		return 0
	}
	// `edc <command> --help`도 같은 상세 화면으로 보낸다. flag의 기본 usage와 어긋나지 않게 한다.
	if len(args) > 1 && (args[1] == "--help" || args[1] == "-h") && printCommandHelp(os.Stdout, args[0]) {
		return 0
	}
	switch args[0] {
	case "help", "-h", "--help":
		if len(args) > 1 {
			if !printCommandHelp(os.Stdout, args[1]) {
				fmt.Fprintf(os.Stderr, "알 수 없는 command: %s\n", args[1])
				return 2
			}
			return 0
		}
		printHelp(os.Stdout)
		return 0
	case "version":
		printVersion(os.Stdout, version)
		return 0
	case "top":
		return runTop(args[1:])
	case "info":
		return runInfo(args[1:], version)
	case "doctor":
		return runDoctor(args[1:], version)
	case "dns":
		return runDNS(args[1:], version)
	case "tcp":
		return runTCP(args[1:], version)
	case "tls":
		return runTLS(args[1:], version)
	case "http":
		return runHTTP(args[1:], version)
	case "net":
		return runNet(args[1:], version)
	case "where":
		return runWhere(args[1:], version)
	case "sockets":
		return runSimple(args[1:], version, "sockets", "sockets", probeSockets)
	case "quality":
		return runSimple(args[1:], version, "quality", "net.quality", probeQuality)
	case "capture":
		return runCapture(args[1:])
	case "report":
		return runReport(args[1:])
	case "remote":
		return runRemote(args[1:], version)
	case "completion":
		return runCompletion(args[1:])
	case "update":
		return runUpdate(args[1:], version)
	default:
		fmt.Fprintf(os.Stderr, "알 수 없는 command: %s\n", args[0])
		fmt.Fprintln(os.Stderr, "edc help로 명령 목록을 봅니다")
		return 2
	}
}

func runDoctor(args []string, version string) int {
	options := commonOptions{timeout: 15 * time.Second, redact: true}
	set := flag.NewFlagSet("doctor", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	profile := set.String("profile", "default", "default 또는 full")
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "사용법: edc doctor [options] <host|URL>")
		return 2
	}
	if *profile != "default" && *profile != "full" {
		fmt.Fprintln(os.Stderr, "--profile은 default 또는 full이어야 합니다")
		return 2
	}
	host, address, rawURL, err := normalizeTarget(set.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	started := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deadline, cancelDeadline := context.WithTimeout(ctx, options.timeout)
	defer cancelDeadline()
	probes := []doctorProbe{
		{name: "net.interfaces", run: func(context.Context) Result { return probeInterfaces() }},
		{name: "net.route", run: func(ctx context.Context) Result { return probeRoute(ctx, host) }},
		{name: "dns.config", run: probeDNSConfig},
		{name: "dns.lookup", run: func(ctx context.Context) Result { return probeDNS(ctx, host) }},
		{name: "net.ping", run: func(ctx context.Context) Result { return probePing(ctx, host) }},
		{name: "tcp.check", run: func(ctx context.Context) Result { return probeTCP(ctx, address) }},
		{name: "tls.check", run: func(ctx context.Context) Result {
			if strings.HasPrefix(rawURL, "http://") {
				return unsupported("tls.check", "HTTP target에는 TLS를 적용하지 않습니다")
			}
			return probeTLS(ctx, address, host)
		}},
		{name: "http.check", run: func(ctx context.Context) Result { return probeHTTP(ctx, rawURL) }},
		{name: "sockets", run: probeSockets},
	}
	if *profile == "full" {
		probes = append(probes, doctorProbe{name: "net.quality", run: probeQuality})
	}
	target := map[string]interface{}{"input": set.Arg(0), "host": host, "address": address, "url": rawURL}
	if options.jsonPath == "" && liveTerminal() {
		return runDoctorLive(deadline, cancel, probes, options, version, started, set.Arg(0), target)
	}
	results := runParallel(deadline, doctorProbeFuncs(probes))
	return emit(options, buildReport(version, started, target, results, options.redact))
}

func runDNS(args []string, version string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "사용법: edc dns <lookup|config> ...")
		return 2
	}
	switch args[0] {
	case "lookup":
		return runTargetProbe(args[1:], version, "dns lookup", "dns.lookup", probeDNS)
	case "config":
		return runSimple(args[1:], version, "dns config", "dns.config", probeDNSConfig)
	default:
		fmt.Fprintln(os.Stderr, "사용법: edc dns <lookup|config> ...")
		return 2
	}
}

func runTCP(args []string, version string) int {
	if len(args) == 0 || args[0] != "check" {
		fmt.Fprintln(os.Stderr, "사용법: edc tcp check <host:port>")
		return 2
	}
	return runTargetProbe(args[1:], version, "tcp check", "tcp.check", func(ctx context.Context, target string) Result {
		if _, _, err := net.SplitHostPort(target); err != nil {
			return resultFromError("tcp.check", time.Now(), "input", fmt.Errorf("host:port 형식이 필요합니다: %s", target))
		}
		return probeTCP(ctx, target)
	})
}

func runTLS(args []string, version string) int {
	if len(args) == 0 || args[0] != "check" {
		fmt.Fprintln(os.Stderr, "사용법: edc tls check [--min-days N] <host:port>")
		return 2
	}
	minDays := 0
	flags := probeFlags{
		bind: func(set *flag.FlagSet) {
			set.IntVar(&minDays, "min-days", 0, "인증서 남은 일수가 이 값보다 작으면 fail, 0은 비활성")
		},
		check: func() error {
			if minDays < 0 {
				return errors.New("--min-days는 0 이상이어야 합니다")
			}
			return nil
		},
	}
	return runTargetProbeWithFlags(args[1:], version, "tls check", "tls.check", flags, func(ctx context.Context, target string) Result {
		host, _, err := net.SplitHostPort(target)
		if err != nil {
			return resultFromError("tls.check", time.Now(), "input", fmt.Errorf("host:port 형식이 필요합니다: %s", target))
		}
		return probeTLSWithOptions(ctx, target, host, tlsCheckOptions{minDays: minDays})
	})
}

func runHTTP(args []string, version string) int {
	if len(args) == 0 || args[0] != "check" {
		fmt.Fprintln(os.Stderr, "사용법: edc http check [--expect-status N] <URL>")
		return 2
	}
	expectStatus := 0
	flags := probeFlags{
		bind: func(set *flag.FlagSet) {
			set.IntVar(&expectStatus, "expect-status", 0, "기대하는 HTTP status code, 다르면 fail, 0은 기본 규칙")
		},
		check: func() error {
			if expectStatus != 0 && (expectStatus < 100 || expectStatus > 599) {
				return errors.New("--expect-status는 100에서 599 사이여야 합니다")
			}
			return nil
		},
	}
	return runTargetProbeWithFlags(args[1:], version, "http check", "http.check", flags, func(ctx context.Context, target string) Result {
		return probeHTTPWithOptions(ctx, target, httpCheckOptions{expectStatus: expectStatus})
	})
}

func runNet(args []string, version string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "사용법: edc net <interfaces|route|ping|trace>")
		return 2
	}
	switch args[0] {
	case "interfaces":
		return runSimple(args[1:], version, "net interfaces", "net.interfaces", func(context.Context) Result { return probeInterfaces() })
	case "route":
		return runTargetProbe(args[1:], version, "net route", "net.route", probeRoute)
	case "ping":
		return runTargetProbe(args[1:], version, "net ping", "net.ping", probePing)
	case "trace":
		return runTargetProbe(args[1:], version, "net trace", "net.trace", probeTrace)
	default:
		fmt.Fprintln(os.Stderr, "사용법: edc net <interfaces|route|ping|trace>")
		return 2
	}
}

// probeFlags는 command별 추가 flag를 공통 flag 옆에 붙이고 parse 뒤에 값을 검사한다.
type probeFlags struct {
	bind  func(*flag.FlagSet)
	check func() error
}

func runTargetProbe(args []string, version, name, probeID string, probe func(context.Context, string) Result) int {
	return runTargetProbeWithFlags(args, version, name, probeID, probeFlags{}, probe)
}

func runTargetProbeWithFlags(args []string, version, name, probeID string, extra probeFlags, probe func(context.Context, string) Result) int {
	options := commonOptions{timeout: 15 * time.Second, redact: true}
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	if extra.bind != nil {
		extra.bind(set)
	}
	if err := set.Parse(args); err != nil {
		return 2
	}
	if extra.check != nil {
		if err := extra.check(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	if set.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "%s에는 target 하나가 필요합니다\n", name)
		return 2
	}
	started := time.Now()
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	target := map[string]interface{}{"input": set.Arg(0)}
	run := func(ctx context.Context) Result { return probe(ctx, set.Arg(0)) }
	if options.jsonPath == "" && liveTerminal() {
		return runProbeLive(ctx, cancel, probeID, set.Arg(0), options, version, started, target, run)
	}
	return emit(options, buildReport(version, started, target, []Result{run(ctx)}, options.redact))
}

// probeContext는 사용자 취소용 cancel과 timeout을 분리해 돌려준다.
func probeContext(timeout time.Duration) (context.Context, context.CancelFunc, context.CancelFunc) {
	base, cancel := context.WithCancel(context.Background())
	ctx, deadline := context.WithTimeout(base, timeout)
	return ctx, cancel, deadline
}

func runSimple(args []string, version, name, probeID string, probe func(context.Context) Result) int {
	options := commonOptions{timeout: 15 * time.Second, redact: true}
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "%s는 positional argument를 받지 않습니다\n", name)
		return 2
	}
	started := time.Now()
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	if options.jsonPath == "" && liveTerminal() {
		return runProbeLive(ctx, cancel, probeID, "", options, version, started, nil, probe)
	}
	return emit(options, buildReport(version, started, nil, []Result{probe(ctx)}, options.redact))
}

func bindCommon(set *flag.FlagSet, options *commonOptions) {
	set.StringVar(&options.jsonPath, "json", "", "JSON 출력 경로, stdout은 -")
	set.DurationVar(&options.timeout, "timeout", options.timeout, "실행 제한 시간")
	set.BoolVar(&options.verbose, "verbose", false, "상세 evidence 출력")
	set.BoolVar(&options.verbose, "v", false, "--verbose 단축 option")
	set.BoolVar(&options.redact, "redact", options.redact, "JSON 민감정보 redaction")
}

func emit(options commonOptions, report Report) int {
	if options.jsonPath == "" {
		printTerminalWithColor(os.Stdout, report.Results, options.verbose, isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == "")
	} else if err := writeJSONOutput(options.jsonPath, report); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	return exitCode(report.Results)
}

func runParallel(ctx context.Context, probes []func(context.Context) Result) []Result {
	return runParallelWith(ctx, probes, nil)
}

// runParallelWith는 probe를 병렬로 실행하고, observe가 있으면 결과가 나올 때마다 probe goroutine에서 호출한다.
func runParallelWith(ctx context.Context, probes []func(context.Context) Result, observe func(index int, result Result)) []Result {
	results := make([]Result, 0, len(probes))
	channel := make(chan Result, len(probes))
	var group sync.WaitGroup
	for index, probe := range probes {
		group.Add(1)
		go func(index int, run func(context.Context) Result) {
			defer group.Done()
			result := run(ctx)
			if observe != nil {
				observe(index, result)
			}
			channel <- result
		}(index, probe)
	}
	go func() { group.Wait(); close(channel) }()
	for result := range channel {
		results = append(results, result)
	}
	sortResults(results)
	return results
}

func runReport(args []string) int {
	const usage = "사용법: edc report show <file> | edc report diff [--json <path|->] <before> <after>"
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "show":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, usage)
			return 2
		}
		return runReportShow(args[1])
	case "diff":
		return runReportDiff(args[1:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
}
