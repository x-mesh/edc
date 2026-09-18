package edc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	changeKindAuthorizedKeys = "authorized-keys"
	changeKindIPTables       = "iptables"
	changeStateDirectory     = "/run/edc"
	changeStateSchemaVersion = 2
	changeDefaultSeconds     = 120
	changeMinSeconds         = 10
	changeMaxSeconds         = 900
)

type changeRunner interface {
	routeRunner
	runInput(context.Context, []byte, string, ...string) (string, error)
}

type changeSnapshot struct {
	SchemaVersion int        `json:"schema_version"`
	RunID         string     `json:"run_id"`
	CreatedAt     time.Time  `json:"created_at"`
	Kind          string     `json:"kind"`
	Path          string     `json:"path,omitempty"`
	Existed       bool       `json:"existed"`
	Mode          uint32     `json:"mode,omitempty"`
	Data          []byte     `json:"data"`
	AppliedData   []byte     `json:"applied_data"`
	AppliedState  []byte     `json:"applied_state,omitempty"`
	UnitName      string     `json:"unit_name"`
	Seconds       int        `json:"seconds"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
}

type changeInput struct {
	kind        string
	path        string
	contentFile string
	rulesFile   string
	seconds     int
	yes         bool
	statePath   string
	edcPath     string
	execID      string
}

func changeStatePath(execID string) string {
	return filepath.Join(changeStateDirectory, "change-"+execID+".json")
}

func changeUnitName(execID string) string {
	return "edc-change-rollback-" + execID
}

func normalizeChangeKind(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case changeKindAuthorizedKeys:
		return changeKindAuthorizedKeys, true
	case changeKindIPTables:
		return changeKindIPTables, true
	default:
		return "", false
	}
}

func runChange(args []string, version string) int {
	usage := T("cli.usage", "edc change <apply|status|confirm|rollback> ...")
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "apply":
		return runChangeApply(args[1:], version)
	case "status":
		return runChangeStatus(args[1:], version)
	case "confirm":
		return runChangeStateCommand(args[1:], version, "confirm")
	case "rollback":
		return runChangeStateCommand(args[1:], version, "rollback")
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
}

func runChangeApply(args []string, version string) int {
	options := configuredCommon(60 * time.Second)
	set := flag.NewFlagSet("change apply", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	kind := set.String("kind", "", T("change.flag.kind"))
	path := set.String("path", "", T("change.flag.path"))
	contentFile := set.String("content-file", "", T("change.flag.content_file"))
	rulesFile := set.String("rules-file", "", T("change.flag.rules_file"))
	seconds := set.Int("seconds", changeDefaultSeconds, T("change.flag.seconds"))
	yes := set.Bool("yes", false, T("change.flag.yes"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.error.no_positional", "change apply"))
		return 2
	}
	normalizedKind, ok := normalizeChangeKind(*kind)
	if !ok {
		fmt.Fprintln(os.Stderr, T("change.error.kind"))
		return 2
	}
	if *seconds < changeMinSeconds || *seconds > changeMaxSeconds {
		fmt.Fprintln(os.Stderr, T("change.error.seconds_range", changeMinSeconds, changeMaxSeconds))
		return 2
	}
	if err := validateChangeFiles(normalizedKind, *path, *contentFile, *rulesFile); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, T("change.error.root_required"))
		return 3
	}
	runner, supported := newChangeRunner()
	if !supported {
		return emit(options, buildReport(version, time.Now(), nil, []Result{unsupported("change.apply", T("change.skip.linux_only"))}, options.redact))
	}
	started := time.Now()
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	execID := runID(started)
	input := changeInput{kind: normalizedKind, path: *path, contentFile: *contentFile, rulesFile: *rulesFile, seconds: *seconds, yes: *yes, execID: execID, statePath: changeStatePath(execID)}
	input.edcPath, _ = os.Executable()
	result := executeChangeApply(ctx, runner, input, started)
	return emit(options, buildReport(version, started, nil, []Result{result}, options.redact))
}

func validateChangeFiles(kind, path, contentFile, rulesFile string) error {
	switch kind {
	case changeKindAuthorizedKeys:
		if path == "" || contentFile == "" {
			return errors.New(T("change.error.authorized_keys_args"))
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		base := filepath.Base(absolute)
		if base != "authorized_keys" && base != "authorized_keys2" {
			return errors.New(T("change.error.authorized_keys_path"))
		}
		if _, err := os.Stat(contentFile); err != nil {
			return fmt.Errorf(T("change.error.input_unreadable"), contentFile, err)
		}
	case changeKindIPTables:
		if rulesFile == "" {
			return errors.New(T("change.error.rules_required"))
		}
		if _, err := os.Stat(rulesFile); err != nil {
			return fmt.Errorf(T("change.error.input_unreadable"), rulesFile, err)
		}
	}
	return nil
}

func executeChangeApply(ctx context.Context, runner changeRunner, input changeInput, started time.Time) Result {
	var result Result
	if err := withChangeApplyLock(func() {
		result = executeChangeApplyLocked(ctx, runner, input, started)
	}); err != nil {
		return resultFromError("change.apply", started, "state", err)
	}
	return result
}

func executeChangeApplyLocked(ctx context.Context, runner changeRunner, input changeInput, started time.Time) Result {
	probe := "change.apply"
	if pending, err := hasPendingChange(); err != nil {
		return resultFromError(probe, started, "state", err)
	} else if pending {
		return resultFromError(probe, started, "state", errors.New(T("change.error.pending")))
	}
	snapshot, err := prepareChangeSnapshot(ctx, runner, input)
	if err != nil {
		return resultFromError(probe, started, "snapshot", err)
	}
	if err := writeChangeState(input.statePath, snapshot); err != nil {
		return resultFromError(probe, started, "state", err)
	}
	guard := armChangeGuard(ctx, runner, snapshot.UnitName, snapshot.Seconds, input.statePath, input.edcPath)
	if !guard.ArmSucceeded || !guard.IsActive {
		_ = disarmGuard(ctx, runner, snapshot.UnitName)
		_ = markChangeCompleted(input.statePath, snapshot)
		return resultFromError(probe, started, "guard", errors.New(T("change.error.guard_failed")))
	}
	if err := applyChange(ctx, runner, input, snapshot); err != nil {
		return rollbackChangeAfterApplyFailure(ctx, runner, input, snapshot, probe, started, err)
	}
	if snapshot.Kind == changeKindIPTables {
		appliedState, err := runner.run(ctx, "iptables-save")
		if err != nil {
			return rollbackChangeAfterApplyFailure(ctx, runner, input, snapshot, probe, started, fmt.Errorf("iptables-save after apply: %w", err))
		}
		snapshot.AppliedState = []byte(appliedState)
		if err := writeChangeState(input.statePath, snapshot); err != nil {
			return rollbackChangeAfterApplyFailure(ctx, runner, input, snapshot, probe, started, fmt.Errorf("save applied state: %w", err))
		}
	}
	if input.yes {
		if err := confirmChange(ctx, runner, input.statePath); err != nil {
			return resultFromError(probe, started, "confirm", err)
		}
		return Result{Probe: probe, Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(), Summary: T("change.apply.confirmed", snapshot.Kind)}
	}
	return Result{Probe: probe, Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(), Summary: T("change.apply.pending", snapshot.Kind, snapshot.RunID, snapshot.Seconds), Metrics: map[string]interface{}{"kind": snapshot.Kind, "run_id": snapshot.RunID, "state": input.statePath, "unit": snapshot.UnitName, "seconds": snapshot.Seconds, "confirmed": false}}
}

func rollbackChangeAfterApplyFailure(ctx context.Context, runner changeRunner, input changeInput, snapshot changeSnapshot, probe string, started time.Time, applyErr error) Result {
	if snapshot.Kind == changeKindIPTables && len(snapshot.AppliedState) == 0 {
		if current, err := runner.run(ctx, "iptables-save"); err == nil {
			snapshot.AppliedState = []byte(current)
			_ = writeChangeState(input.statePath, snapshot)
		}
	}
	if rollbackErr := rollbackChange(ctx, runner, snapshot); rollbackErr == nil {
		_ = disarmGuard(ctx, runner, snapshot.UnitName)
		_ = markChangeCompleted(input.statePath, snapshot)
	} else {
		return resultFromError(probe, started, "apply", fmt.Errorf("%w: %v", applyErr, rollbackErr))
	}
	return resultFromError(probe, started, "apply", applyErr)
}

func prepareChangeSnapshot(ctx context.Context, runner changeRunner, input changeInput) (changeSnapshot, error) {
	snapshot := changeSnapshot{SchemaVersion: changeStateSchemaVersion, RunID: input.execID, CreatedAt: time.Now().UTC(), Kind: input.kind, UnitName: changeUnitName(input.execID), Seconds: input.seconds}
	switch input.kind {
	case changeKindAuthorizedKeys:
		content, err := os.ReadFile(input.contentFile)
		if err != nil {
			return changeSnapshot{}, err
		}
		if len(bytesTrimSpace(content)) == 0 || bytesContainsPrivateKey(content) {
			return changeSnapshot{}, errors.New(T("change.error.invalid_authorized_keys"))
		}
		path, err := filepath.Abs(input.path)
		if err != nil {
			return changeSnapshot{}, err
		}
		before, err := readChangeFile(path)
		if err != nil {
			return changeSnapshot{}, err
		}
		snapshot.Path, snapshot.Existed, snapshot.Mode, snapshot.Data = path, before.Existed, before.Mode, before.Data
		snapshot.AppliedData = append([]byte(nil), content...)
	case changeKindIPTables:
		rules, err := os.ReadFile(input.rulesFile)
		if err != nil {
			return changeSnapshot{}, err
		}
		if len(bytesTrimSpace(rules)) == 0 {
			return changeSnapshot{}, errors.New(T("change.error.rules_empty"))
		}
		if _, err := runner.runInput(ctx, rules, "iptables-restore", "--test"); err != nil {
			return changeSnapshot{}, fmt.Errorf("%s: %w", T("change.error.rules_invalid"), err)
		}
		output, err := runner.run(ctx, "iptables-save")
		if err != nil {
			return changeSnapshot{}, fmt.Errorf("iptables-save: %w", err)
		}
		snapshot.Data = []byte(output)
		snapshot.AppliedData = append([]byte(nil), rules...)
	}
	return snapshot, nil
}

type changeFile struct {
	Existed bool
	Mode    uint32
	Data    []byte
}

func readChangeFile(path string) (changeFile, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return changeFile{}, nil
	}
	if err != nil {
		return changeFile{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return changeFile{}, errors.New(T("change.error.target_not_regular"))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return changeFile{}, err
	}
	return changeFile{Existed: true, Mode: uint32(info.Mode().Perm()), Data: data}, nil
}

func applyChange(ctx context.Context, runner changeRunner, input changeInput, snapshot changeSnapshot) error {
	switch input.kind {
	case changeKindAuthorizedKeys:
		return writeChangeFile(snapshot.Path, snapshot.AppliedData, snapshot.Mode)
	case changeKindIPTables:
		if _, err := runner.runInput(ctx, snapshot.AppliedData, "iptables-restore"); err != nil {
			return fmt.Errorf("iptables-restore: %w", err)
		}
		return nil
	default:
		return errors.New(T("change.error.kind"))
	}
}

func rollbackChange(ctx context.Context, runner changeRunner, snapshot changeSnapshot) error {
	switch snapshot.Kind {
	case changeKindAuthorizedKeys:
		current, err := readChangeFile(snapshot.Path)
		if err != nil {
			return err
		}
		if !snapshot.Existed {
			if !current.Existed {
				return nil
			}
			if !bytes.Equal(current.Data, snapshot.AppliedData) || current.Mode != changeAppliedMode(snapshot) {
				return errors.New(T("change.error.rollback_target_changed"))
			}
			if err := os.Remove(snapshot.Path); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		if current.Existed && bytes.Equal(current.Data, snapshot.Data) && current.Mode == snapshot.Mode {
			return nil
		}
		if !current.Existed || !bytes.Equal(current.Data, snapshot.AppliedData) || current.Mode != snapshot.Mode {
			return errors.New(T("change.error.rollback_target_changed"))
		}
		if err := writeChangeFile(snapshot.Path, snapshot.Data, snapshot.Mode); err != nil {
			return err
		}
		current, err = readChangeFile(snapshot.Path)
		if err != nil {
			return err
		}
		if !current.Existed || !bytes.Equal(current.Data, snapshot.Data) || current.Mode != snapshot.Mode {
			return errors.New(T("change.error.rollback_verify"))
		}
		return nil
	case changeKindIPTables:
		current, err := runner.run(ctx, "iptables-save")
		if err != nil {
			return fmt.Errorf("iptables-save: %w", err)
		}
		if strings.TrimSpace(current) == strings.TrimSpace(string(snapshot.Data)) {
			return nil
		}
		if len(snapshot.AppliedState) > 0 && strings.TrimSpace(current) != strings.TrimSpace(string(snapshot.AppliedState)) {
			return errors.New(T("change.error.rollback_target_changed"))
		}
		if _, err := runner.runInput(ctx, snapshot.Data, "iptables-restore"); err != nil {
			return fmt.Errorf("iptables-restore: %w", err)
		}
		current, err = runner.run(ctx, "iptables-save")
		if err != nil {
			return fmt.Errorf("iptables-save: %w", err)
		}
		if strings.TrimSpace(current) != strings.TrimSpace(string(snapshot.Data)) {
			return errors.New(T("change.error.rollback_verify"))
		}
		return nil
	default:
		return errors.New(T("change.error.kind"))
	}
}

func changeAppliedMode(snapshot changeSnapshot) uint32 {
	if snapshot.Mode != 0 {
		return snapshot.Mode
	}
	return 0o600
}

func writeChangeFile(path string, data []byte, mode uint32) error {
	if _, err := readChangeFile(path); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".edc-change-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if mode == 0 {
		mode = 0o600
	}
	if err := temp.Chmod(os.FileMode(mode)); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func runChangeStateCommand(args []string, version, name string) int {
	options := configuredCommon(30 * time.Second)
	set := flag.NewFlagSet("change "+name, flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	statePath := set.String("state", "", T("change.flag.state"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.error.no_positional", "change "+name))
		return 2
	}
	if *statePath == "" {
		fmt.Fprintln(os.Stderr, T("change.error.state_required"))
		return 2
	}
	validatedStatePath, err := validateChangeStatePath(*statePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	*statePath = validatedStatePath
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, T("change.error.root_required"))
		return 3
	}
	runner, supported := newChangeRunner()
	if !supported {
		return emit(options, buildReport(version, time.Now(), nil, []Result{unsupported("change."+name, T("change.skip.linux_only"))}, options.redact))
	}
	started := time.Now()
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	var commandErr error
	if name == "confirm" {
		commandErr = confirmChange(ctx, runner, *statePath)
	} else {
		commandErr = rollbackState(ctx, runner, *statePath)
	}
	if commandErr != nil {
		return emit(options, buildReport(version, started, nil, []Result{resultFromError("change."+name, started, "change", commandErr)}, options.redact))
	}
	return emit(options, buildReport(version, started, nil, []Result{{Probe: "change." + name, Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(), Summary: T("change." + name + ".done")}}, options.redact))
}

func confirmChange(ctx context.Context, runner changeRunner, statePath string) error {
	return withChangeLock(statePath, func() error {
		snapshot, err := readChangeState(statePath)
		if err != nil {
			return err
		}
		if snapshot.CompletedAt != nil {
			return errors.New(T("change.error.already_completed"))
		}
		if warning := disarmGuard(ctx, runner, snapshot.UnitName); warning != "" {
			return errors.New(warning)
		}
		return markChangeCompleted(statePath, snapshot)
	})
}

func rollbackState(ctx context.Context, runner changeRunner, statePath string) error {
	return withChangeLock(statePath, func() error {
		snapshot, err := readChangeState(statePath)
		if err != nil {
			return err
		}
		if snapshot.CompletedAt != nil {
			return errors.New(T("change.error.already_completed"))
		}
		if err := rollbackChange(ctx, runner, snapshot); err != nil {
			return err
		}
		_ = disarmGuard(ctx, runner, snapshot.UnitName)
		return markChangeCompleted(statePath, snapshot)
	})
}

func markChangeCompleted(path string, snapshot changeSnapshot) error {
	completedAt := time.Now().UTC()
	snapshot.CompletedAt = &completedAt
	return writeChangeState(path, snapshot)
}

func armChangeGuard(ctx context.Context, runner changeRunner, unit string, seconds int, statePath, edcPath string) guardProbe {
	_, err := runner.run(ctx, "systemd-run", changeSystemdRunArgs(unit, seconds, statePath, edcPath)...)
	probe := guardProbe{ArmAttempted: true, ArmSucceeded: err == nil}
	if !probe.ArmSucceeded {
		return probe
	}
	output, _ := runner.run(ctx, "systemctl", "is-active", unit+".timer")
	probe.IsActive = parseIsActive(output)
	return probe
}

func changeSystemdRunArgs(unit string, seconds int, statePath, edcPath string) []string {
	return []string{"--collect", fmt.Sprintf("--on-active=%d", seconds), "--timer-property=AccuracySec=1s", "--unit=" + unit, edcPath, "change", "rollback", "--state", statePath}
}

func hasPendingChange() (bool, error) {
	paths, err := listChangeStateFiles(changeStateDirectory)
	if err != nil {
		return false, err
	}
	for _, path := range paths {
		snapshot, err := readChangeState(path)
		if err != nil {
			return false, err
		}
		if snapshot.CompletedAt == nil {
			return true, nil
		}
	}
	return false, nil
}

func runChangeStatus(args []string, version string) int {
	options := configuredCommon(15 * time.Second)
	set := flag.NewFlagSet("change status", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.error.no_positional", "change status"))
		return 2
	}
	started := time.Now()
	runner, supported := newChangeRunner()
	if !supported {
		return emit(options, buildReport(version, started, nil, []Result{unsupported("change.status", T("change.skip.linux_only"))}, options.redact))
	}
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	paths, err := listChangeStateFiles(changeStateDirectory)
	if err != nil {
		return emit(options, buildReport(version, started, nil, []Result{resultFromError("change.status", started, "state", err)}, options.redact))
	}
	if len(paths) == 0 {
		return emit(options, buildReport(version, started, nil, []Result{{Probe: "change.status", Status: StatusSkip, StartedAt: started.UTC(), Summary: T("change.status.none")}}, options.redact))
	}
	results := make([]Result, 0, len(paths))
	for _, path := range paths {
		results = append(results, changeStatusForFile(ctx, runner, path))
	}
	return emit(options, buildReport(version, started, nil, results, options.redact))
}

func listChangeStateFiles(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "change-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		paths = append(paths, filepath.Join(directory, entry.Name()))
	}
	sort.Strings(paths)
	return paths, nil
}

func validateChangeStatePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	name := filepath.Base(absolute)
	if filepath.Dir(absolute) != changeStateDirectory || !strings.HasPrefix(name, "change-") || !strings.HasSuffix(name, ".json") {
		return "", errors.New(T("change.error.state_path"))
	}
	return absolute, nil
}

func changeStatusForFile(ctx context.Context, runner changeRunner, path string) Result {
	started := time.Now()
	snapshot, err := readChangeState(path)
	if err != nil {
		return resultFromError("change.status", started, "state", err)
	}
	status := StatusPass
	var warnings []string
	if snapshot.CompletedAt == nil {
		output, runErr := runner.run(ctx, "systemctl", "is-active", snapshot.UnitName+".timer")
		if runErr != nil || !parseIsActive(output) {
			status = StatusWarn
			warnings = append(warnings, T("change.status.timer_mismatch", snapshot.UnitName))
		}
	}
	remaining := "n/a"
	if snapshot.CompletedAt == nil {
		remaining = time.Until(snapshot.CreatedAt.Add(time.Duration(snapshot.Seconds) * time.Second)).Round(time.Second).String()
	}
	summary := T("change.status.pending", snapshot.Kind, snapshot.RunID)
	if snapshot.CompletedAt != nil {
		summary = T("change.status.completed", snapshot.Kind, snapshot.RunID)
	}
	return Result{Probe: "change.status." + snapshot.RunID, Status: status, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(), Summary: summary, Metrics: map[string]interface{}{"kind": snapshot.Kind, "run_id": snapshot.RunID, "state": path, "unit": snapshot.UnitName, "remaining": remaining, "completed": snapshot.CompletedAt != nil}, Warnings: warnings}
}

func writeChangeState(path string, snapshot changeSnapshot) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(directory, ".change-state-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func readChangeState(path string) (changeSnapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return changeSnapshot{}, err
	}
	var snapshot changeSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return changeSnapshot{}, err
	}
	if snapshot.SchemaVersion != changeStateSchemaVersion || snapshot.RunID == "" || snapshot.UnitName != changeUnitName(snapshot.RunID) || snapshot.CreatedAt.IsZero() || snapshot.Seconds < changeMinSeconds || snapshot.Seconds > changeMaxSeconds {
		return changeSnapshot{}, errors.New(T("change.error.invalid_state"))
	}
	if _, ok := normalizeChangeKind(snapshot.Kind); !ok {
		return changeSnapshot{}, errors.New(T("change.error.invalid_state"))
	}
	if snapshot.Kind == changeKindAuthorizedKeys && (!filepath.IsAbs(snapshot.Path) || (filepath.Base(snapshot.Path) != "authorized_keys" && filepath.Base(snapshot.Path) != "authorized_keys2") || len(snapshot.AppliedData) == 0) {
		return changeSnapshot{}, errors.New(T("change.error.invalid_state"))
	}
	if snapshot.Kind == changeKindIPTables && (len(snapshot.Data) == 0 || len(snapshot.AppliedData) == 0) {
		return changeSnapshot{}, errors.New(T("change.error.invalid_state"))
	}
	return snapshot, nil
}

func withChangeLock(statePath string, action func() error) error {
	return withChangeFileLock(statePath+".lock", action)
}

func withChangeApplyLock(action func()) error {
	return withChangeFileLock(filepath.Join(changeStateDirectory, "change-apply.lock"), func() error {
		action()
		return nil
	})
}

func withChangeFileLock(lockPath string, action func() error) error {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(lockPath), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("%s: %w", T("change.error.locked"), err)
	}
	defer func() {
		lock.Close()
		_ = os.Remove(lockPath)
	}()
	return action()
}

func bytesTrimSpace(value []byte) []byte { return []byte(strings.TrimSpace(string(value))) }

func bytesContainsPrivateKey(value []byte) bool {
	text := string(value)
	return strings.Contains(text, "PRIVATE KEY")
}
