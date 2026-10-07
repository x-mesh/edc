package edc

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const fsWatchUsage = "edc watch fs [directory] [options]"

// fsWatchDefaultExcludes는 VCS, 의존성과 cache 디렉터리다. macOS kqueue는 감시하는 파일마다 fd를 하나 연다.
// build와 dist는 감시하려는 산출물일 수 있어서 넣지 않는다.
var fsWatchDefaultExcludes = []string{
	"**/.git/**", "**/node_modules/**", "**/.venv/**", "**/venv/**", "**/__pycache__/**",
	"**/.mypy_cache/**", "**/.pytest_cache/**", "**/.ruff_cache/**", "**/.tox/**", "**/.next/**", "**/.gradle/**",
}

type fsWatchRule struct {
	Name         string   `yaml:"name"`
	Events       []string `yaml:"events"`
	Match        string   `yaml:"match"`
	Command      []string `yaml:"command"`
	CWD          string   `yaml:"cwd"`
	DebounceText string   `yaml:"debounce"`
	TimeoutText  string   `yaml:"timeout"`
	debounce     time.Duration
	timeout      time.Duration
}

type fsWatchConfig struct {
	Directory string        `yaml:"directory"`
	Recursive bool          `yaml:"recursive"`
	Exclude   []string      `yaml:"exclude"`
	Rules     []fsWatchRule `yaml:"rules"`
}

type fsWatchOptions struct {
	root       string
	recursive  bool
	exclude    []string
	events     []string
	match      string
	rules      []fsWatchRule
	duration   time.Duration
	jsonPath   string
	dryRun     bool
	outputPath string
	outputInfo os.FileInfo
	status     *fsWatchStatus
}

type fsWatchExcludes []string

func (values *fsWatchExcludes) String() string         { return strings.Join(*values, ",") }
func (values *fsWatchExcludes) Set(value string) error { *values = append(*values, value); return nil }

func parseFSWatchOptions(args []string, output io.Writer) (fsWatchOptions, error) {
	options := fsWatchOptions{match: "**"}
	set := flag.NewFlagSet("watch fs", flag.ContinueOnError)
	set.SetOutput(output)
	set.BoolVar(&options.recursive, "recursive", false, T("watchfs.option.recursive"))
	eventText := set.String("event", "create,modify,remove,rename", T("watchfs.option.event"))
	set.StringVar(&options.match, "match", "**", T("watchfs.option.match"))
	command := set.String("exec", "", T("watchfs.option.exec"))
	rulesPath := set.String("rules", "", T("watchfs.option.rules"))
	debounce := set.Duration("debounce", 200*time.Millisecond, T("watchfs.option.debounce"))
	timeout := set.Duration("timeout", 30*time.Second, T("watchfs.option.timeout"))
	set.DurationVar(&options.duration, "duration", 0, T("command.watch.option.duration"))
	set.StringVar(&options.jsonPath, "json", "", T("watchfs.option.json"))
	set.BoolVar(&options.dryRun, "dry-run", false, T("watchfs.option.dry_run"))
	noDefaultExclude := set.Bool("no-default-exclude", false, T("watchfs.option.no_default_exclude"))
	var excludes fsWatchExcludes
	set.Var(&excludes, "exclude", T("watchfs.option.exclude"))
	reordered, err := reorderFSWatchArgs(set, args)
	if err != nil {
		return options, err
	}
	if err := set.Parse(reordered); err != nil {
		return options, err
	}
	if !*noDefaultExclude {
		options.exclude = append(options.exclude, fsWatchDefaultExcludes...)
	}
	if set.NArg() > 1 || options.duration < 0 || *debounce < 0 || *timeout <= 0 {
		return options, errors.New(T("watchfs.invalid_options"))
	}
	if *rulesPath != "" && *command != "" {
		return options, errors.New(T("watchfs.rules_exec_conflict"))
	}
	options.events, err = fsWatchEvents(*eventText)
	if err != nil {
		return options, err
	}
	flagRecursive := options.recursive
	options.root = "."
	var config fsWatchConfig
	if *rulesPath != "" {
		if fsWatchPathsEqual(*rulesPath, options.jsonPath) {
			return options, errors.New(T("watchfs.output_conflict"))
		}
		config, err = loadFSWatchConfig(*rulesPath)
		if err != nil {
			return options, err
		}
		if config.Directory != "" {
			options.root = config.Directory
			if !filepath.IsAbs(options.root) {
				options.root = filepath.Join(filepath.Dir(*rulesPath), options.root)
			}
		}
		options.recursive = config.Recursive
		options.rules = config.Rules
		options.exclude = append(options.exclude, config.Exclude...)
	}
	set.Visit(func(value *flag.Flag) {
		if value.Name == "recursive" {
			options.recursive = flagRecursive
		}
	})
	if set.NArg() == 1 {
		options.root = set.Arg(0)
	}
	options.root, err = filepath.Abs(options.root)
	if err == nil {
		options.root, err = filepath.EvalSymlinks(options.root)
	}
	if err != nil {
		return options, err
	}
	info, err := os.Stat(options.root)
	if err != nil {
		return options, err
	}
	if !info.IsDir() {
		return options, errors.New(T("watchfs.directory_required", options.root))
	}
	if *command != "" {
		options.rules = []fsWatchRule{{Name: "command", Events: options.events, Match: options.match, Command: []string{"/bin/sh", "-c", *command}}}
	}
	options.exclude = append(options.exclude, excludes...)
	for _, pattern := range append(append([]string{options.match}, options.exclude...), fsWatchRulePatterns(options.rules)...) {
		if err := validateFSWatchGlob(pattern); err != nil {
			return options, err
		}
	}
	if len(options.rules) > 128 {
		return options, errors.New(T("watchfs.too_many_rules"))
	}
	names := map[string]bool{}
	for index := range options.rules {
		rule := &options.rules[index]
		if rule.Name == "" {
			rule.Name = fmt.Sprintf("rule-%d", index+1)
		}
		if names[rule.Name] {
			return options, errors.New(T("watchfs.duplicate_rule", rule.Name))
		}
		names[rule.Name] = true
		if len(rule.Command) == 0 || strings.TrimSpace(rule.Command[0]) == "" {
			return options, errors.New(T("watchfs.command_required", rule.Name))
		}
		if rule.Match == "" {
			rule.Match = "**"
		}
		if len(rule.Events) == 0 {
			rule.Events = []string{"create", "modify", "remove", "rename"}
		}
		rule.Events, err = fsWatchEvents(strings.Join(rule.Events, ","))
		if err != nil {
			return options, err
		}
		rule.debounce, rule.timeout = *debounce, *timeout
		if rule.DebounceText != "" {
			rule.debounce, err = time.ParseDuration(rule.DebounceText)
			if err != nil {
				return options, err
			}
		}
		if rule.TimeoutText != "" {
			rule.timeout, err = time.ParseDuration(rule.TimeoutText)
			if err != nil {
				return options, err
			}
		}
		if rule.debounce < 0 || rule.timeout <= 0 {
			return options, errors.New(T("watchfs.invalid_options"))
		}
		if rule.CWD == "" {
			rule.CWD = options.root
		} else if !filepath.IsAbs(rule.CWD) {
			rule.CWD = filepath.Join(options.root, rule.CWD)
		}
		if info, err := os.Stat(rule.CWD); err != nil || !info.IsDir() {
			return options, errors.New(T("watchfs.directory_required", rule.CWD))
		}
	}
	if options.jsonPath != "" && options.jsonPath != "-" {
		options.outputPath, err = filepath.Abs(options.jsonPath)
	}
	return options, err
}

func reorderFSWatchArgs(set *flag.FlagSet, args []string) ([]string, error) {
	var flags, positional []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positional = append(positional, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		name, _, assigned := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if !assigned && (name == "h" || name == "help") {
			return nil, flag.ErrHelp
		}
		option := set.Lookup(name)
		if option == nil {
			return nil, fmt.Errorf("%s: --%s", T("watchfs.unknown_option"), name)
		}
		flags = append(flags, arg)
		boolean, ok := option.Value.(interface{ IsBoolFlag() bool })
		if !assigned && !(ok && boolean.IsBoolFlag()) {
			index++
			if index >= len(args) {
				return nil, fmt.Errorf("%s: --%s", T("watchfs.option_value_required"), name)
			}
			flags = append(flags, args[index])
		}
	}
	if len(positional) > 0 {
		flags = append(flags, "--")
		flags = append(flags, positional...)
	}
	return flags, nil
}

func loadFSWatchConfig(filename string) (fsWatchConfig, error) {
	var config fsWatchConfig
	file, err := os.Open(filename)
	if err != nil {
		return config, err
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil {
		return config, err
	} else if info.Size() > 1<<20 {
		return config, errors.New(T("watchfs.single_document"))
	}
	decoder := yaml.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return config, err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return config, errors.New(T("watchfs.single_document"))
	}
	if len(config.Rules) == 0 {
		return config, errors.New(T("watchfs.rules_required"))
	}
	return config, nil
}

func fsWatchEvents(value string) ([]string, error) {
	var events []string
	seen := map[string]bool{}
	for _, event := range strings.Split(value, ",") {
		event = strings.TrimSpace(event)
		switch event {
		case "create", "modify", "remove", "rename":
		default:
			return nil, errors.New(T("watchfs.invalid_event", event))
		}
		if !seen[event] {
			events = append(events, event)
			seen[event] = true
		}
	}
	return events, nil
}

func fsWatchRulePatterns(rules []fsWatchRule) []string {
	var patterns []string
	for _, rule := range rules {
		if rule.Match != "" {
			patterns = append(patterns, rule.Match)
		}
	}
	return patterns
}

func validateFSWatchGlob(pattern string) error {
	if pattern == "" || strings.HasPrefix(pattern, "/") {
		return errors.New(T("watchfs.invalid_glob", pattern))
	}
	for _, segment := range strings.Split(pattern, "/") {
		if segment == ".." || segment == "" {
			return errors.New(T("watchfs.invalid_glob", pattern))
		}
		if _, err := path.Match(segment, ""); err != nil {
			return errors.New(T("watchfs.invalid_glob", pattern))
		}
	}
	return nil
}

func matchFSWatchGlob(pattern, name string) bool {
	if directory, ok := fsWatchAnyDepthName(pattern); ok {
		for rest := name; ; {
			part, next, found := strings.Cut(rest, "/")
			if part == directory {
				return true
			}
			if !found {
				return false
			}
			rest = next
		}
	}
	patterns, parts := strings.Split(pattern, "/"), strings.Split(name, "/")
	// 시작할 때 모든 파일과 디렉터리를 제외 패턴마다 맞춰 본다. 호출마다 map을 만들면 큰 트리에서 할당과 GC가 시간을 거의 다 쓴다.
	width := len(parts) + 1
	memo := make([]int8, (len(patterns)+1)*width)
	var match func(int, int) bool
	match = func(i, j int) bool {
		key := i*width + j
		if memo[key] != 0 {
			return memo[key] > 0
		}
		result := false
		if i == len(patterns) {
			result = j == len(parts)
		} else if patterns[i] == "**" {
			result = match(i+1, j) || (j < len(parts) && match(i, j+1))
		} else if j < len(parts) {
			ok, _ := path.Match(patterns[i], parts[j])
			result = ok && match(i+1, j+1)
		}
		memo[key] = -1
		if result {
			memo[key] = 1
		}
		return result
	}
	return match(0, 0)
}

// fsWatchAnyDepthName은 "**/<이름>/**" 꼴인 패턴의 이름이다. 기본 제외 패턴이 모두 이 꼴이다.
// 이름에 메타 문자가 없으면 이 패턴은 경로의 한 부분이 그 이름일 때만 맞으므로, 재귀로 맞춰 보지 않는다.
func fsWatchAnyDepthName(pattern string) (string, bool) {
	name, ok := strings.CutPrefix(pattern, "**/")
	if !ok {
		return "", false
	}
	name, ok = strings.CutSuffix(name, "/**")
	if !ok || name == "" || strings.ContainsAny(name, `/*?[\`) {
		return "", false
	}
	return name, true
}

func fsWatchPathsEqual(first, second string) bool {
	if first == "" || second == "" || second == "-" {
		return false
	}
	firstAbs, firstErr := filepath.Abs(first)
	secondAbs, secondErr := filepath.Abs(second)
	if firstErr == nil && secondErr == nil && firstAbs == secondAbs {
		return true
	}
	firstInfo, firstErr := os.Stat(first)
	secondInfo, secondErr := os.Stat(second)
	return firstErr == nil && secondErr == nil && os.SameFile(firstInfo, secondInfo)
}
