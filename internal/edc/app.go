package edc

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
	if len(args) == 0 {
		printHelp(os.Stdout)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		printHelp(os.Stdout)
		return 0
	case "version":
		fmt.Printf("edc %s (schema 1.0)\n", version)
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
	case "sockets":
		return runSimple(args[1:], version, "sockets", func(ctx context.Context) Result { return probeSockets(ctx) })
	case "quality":
		return runSimple(args[1:], version, "quality", func(ctx context.Context) Result { return probeQuality(ctx) })
	case "capture":
		return runCapture(args[1:])
	case "report":
		return runReport(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "알 수 없는 command: %s\n", args[0])
		printHelp(os.Stderr)
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
	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()
	probes := []func(context.Context) Result{
		func(ctx context.Context) Result { return probeInterfaces() },
		func(ctx context.Context) Result { return probeRoute(ctx, host) },
		func(ctx context.Context) Result { return probeDNSConfig(ctx) },
		func(ctx context.Context) Result { return probeDNS(ctx, host) },
		func(ctx context.Context) Result { return probePing(ctx, host) },
		func(ctx context.Context) Result { return probeTCP(ctx, address) },
		func(ctx context.Context) Result {
			if strings.HasPrefix(rawURL, "http://") {
				return unsupported("tls.check", "HTTP target에는 TLS를 적용하지 않습니다")
			}
			return probeTLS(ctx, address, host)
		},
		func(ctx context.Context) Result { return probeHTTP(ctx, rawURL) },
		func(ctx context.Context) Result { return probeSockets(ctx) },
	}
	if *profile == "full" {
		probes = append(probes, func(ctx context.Context) Result { return probeQuality(ctx) })
	}
	results := runParallel(ctx, probes)
	target := map[string]interface{}{"input": set.Arg(0), "host": host, "address": address, "url": rawURL}
	return emit(options, buildReport(version, started, target, results, options.redact))
}

func runDNS(args []string, version string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "사용법: edc dns <lookup|config> ...")
		return 2
	}
	switch args[0] {
	case "lookup":
		return runTargetProbe(args[1:], version, "dns lookup", func(ctx context.Context, target string) Result { return probeDNS(ctx, target) })
	case "config":
		return runSimple(args[1:], version, "dns config", probeDNSConfig)
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
	return runTargetProbe(args[1:], version, "tcp check", func(ctx context.Context, target string) Result {
		if _, _, err := net.SplitHostPort(target); err != nil {
			return resultFromError("tcp.check", time.Now(), "input", fmt.Errorf("host:port 형식이 필요합니다: %s", target))
		}
		return probeTCP(ctx, target)
	})
}

func runTLS(args []string, version string) int {
	if len(args) == 0 || args[0] != "check" {
		fmt.Fprintln(os.Stderr, "사용법: edc tls check <host:port>")
		return 2
	}
	return runTargetProbe(args[1:], version, "tls check", func(ctx context.Context, target string) Result {
		host, _, err := net.SplitHostPort(target)
		if err != nil {
			return resultFromError("tls.check", time.Now(), "input", fmt.Errorf("host:port 형식이 필요합니다: %s", target))
		}
		return probeTLS(ctx, target, host)
	})
}

func runHTTP(args []string, version string) int {
	if len(args) == 0 || args[0] != "check" {
		fmt.Fprintln(os.Stderr, "사용법: edc http check <URL>")
		return 2
	}
	return runTargetProbe(args[1:], version, "http check", func(ctx context.Context, target string) Result { return probeHTTP(ctx, target) })
}

func runNet(args []string, version string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "사용법: edc net <interfaces|route|ping|trace>")
		return 2
	}
	switch args[0] {
	case "interfaces":
		return runSimple(args[1:], version, "net interfaces", func(context.Context) Result { return probeInterfaces() })
	case "route":
		return runTargetProbe(args[1:], version, "net route", probeRoute)
	case "ping":
		return runTargetProbe(args[1:], version, "net ping", probePing)
	case "trace":
		return runTargetProbe(args[1:], version, "net trace", probeTrace)
	default:
		fmt.Fprintln(os.Stderr, "사용법: edc net <interfaces|route|ping|trace>")
		return 2
	}
}

func runTargetProbe(args []string, version, name string, probe func(context.Context, string) Result) int {
	options := commonOptions{timeout: 15 * time.Second, redact: true}
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "%s에는 target 하나가 필요합니다\n", name)
		return 2
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()
	result := probe(ctx, set.Arg(0))
	return emit(options, buildReport(version, started, map[string]interface{}{"input": set.Arg(0)}, []Result{result}, options.redact))
}

func runSimple(args []string, version, name string, probe func(context.Context) Result) int {
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
	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()
	return emit(options, buildReport(version, started, nil, []Result{probe(ctx)}, options.redact))
}

func bindCommon(set *flag.FlagSet, options *commonOptions) {
	set.StringVar(&options.jsonPath, "json", "", "JSON 출력 경로, stdout은 -")
	set.DurationVar(&options.timeout, "timeout", options.timeout, "실행 제한 시간")
	set.BoolVar(&options.verbose, "verbose", false, "상세 evidence 출력")
	set.BoolVar(&options.redact, "redact", options.redact, "JSON 민감정보 redaction")
}

func emit(options commonOptions, report Report) int {
	if options.jsonPath == "" {
		printTerminal(os.Stdout, report.Results, options.verbose)
	} else {
		var writer io.Writer = os.Stdout
		var file *os.File
		if options.jsonPath != "-" {
			var err error
			file, err = os.OpenFile(options.jsonPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			defer file.Close()
			writer = file
		}
		if err := writeJSON(writer, report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	return exitCode(report.Results)
}

func runParallel(ctx context.Context, probes []func(context.Context) Result) []Result {
	results := make([]Result, 0, len(probes))
	channel := make(chan Result, len(probes))
	var group sync.WaitGroup
	for _, probe := range probes {
		group.Add(1)
		go func(run func(context.Context) Result) { defer group.Done(); channel <- run(ctx) }(probe)
	}
	go func() { group.Wait(); close(channel) }()
	for result := range channel {
		results = append(results, result)
	}
	sortResults(results)
	return results
}

func printHelp(writer io.Writer) {
	fmt.Fprint(writer, `edc - macOS 우선 SE/SRE 진단 툴킷

사용법:
  edc top [--interval 1s] [--count N]
  edc info [--public]
  edc doctor [--profile default|full] [options] <host|URL>
  edc net <interfaces|route|ping|trace> ...
  edc dns <lookup|config> ...
  edc tcp check <host:port>
  edc tls check <host:port>
  edc http check <URL>
  edc quality [options]
  edc sockets [options]
  edc capture [options]
  edc report show <file>
  edc version

공통 options: --timeout 15s --json <path|-> --verbose --redact=true
`)
}

func runReport(args []string) int {
	if len(args) != 2 || args[0] != "show" {
		fmt.Fprintln(os.Stderr, "사용법: edc report show <file>")
		return 2
	}
	file, err := os.Open(args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer file.Close()
	var report Report
	decoder := json.NewDecoder(io.LimitReader(file, 20*1024*1024))
	if err := decoder.Decode(&report); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if report.SchemaVersion != "1.0" {
		fmt.Fprintf(os.Stderr, "지원하지 않는 schema version: %s\n", report.SchemaVersion)
		return 2
	}
	printTerminal(os.Stdout, report.Results, false)
	return exitCode(report.Results)
}
