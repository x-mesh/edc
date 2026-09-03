package edc

import (
	"bufio"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var errSetupCancelled = errors.New("setup cancelled")

func runSetup(args []string) int {
	return runSetupWithIO(args, os.Stdin, os.Stdout, os.Stderr, isTerminal(os.Stdin) && isTerminal(os.Stdout), configPath())
}

func runSetupWithIO(args []string, input io.Reader, output, stderr io.Writer, terminal bool, path string) int {
	previousLanguage := currentLanguage()
	defer setLanguage(previousLanguage)
	if len(args) != 0 {
		fmt.Fprintln(stderr, T("cli.usage", "edc setup"))
		return 2
	}
	if !terminal {
		fmt.Fprintln(stderr, T("cli.setup.terminal_required"))
		return 2
	}
	if path == "" {
		fmt.Fprintln(stderr, T("cli.setup.path_unavailable"))
		return 2
	}
	originalSnapshot, err := configFileSnapshot(path)
	if err != nil {
		fmt.Fprintln(stderr, T("cli.setup.input_failed", err))
		return 2
	}
	existing, err := loadConfigAt(path)
	if err != nil {
		fmt.Fprintln(stderr, T("cli.setup.recovering", configError(path, err)))
		existing = edcConfig{}
	}
	recommended := recommendedConfig()
	config := existing
	if config.Lang == "" {
		config.Lang = recommended.Lang
	}
	reader := bufio.NewReader(input)
	fmt.Fprintln(output, T("cli.setup.title"))
	fmt.Fprintln(output, T("cli.setup.path", path))

	language, err := setupValue(reader, output, T("cli.setup.language"), config.Lang, func(value string) error {
		if _, ok := normalizeLanguage(value); !ok {
			return errors.New(T("cli.setup.validation.language"))
		}
		return nil
	})
	if err != nil {
		return setupInputError(stderr, err)
	}
	config.Lang = language
	setLanguage(language)

	sections := []struct {
		key  string
		edit func() error
	}{
		{"common", func() error {
			config.Defaults.Common = mergeConfigSection(config.Defaults.Common, recommended.Defaults.Common)
			return editCommonSetup(reader, output, &config.Defaults.Common)
		}},
		{"log", func() error {
			config.Defaults.Log = mergeConfigSection(config.Defaults.Log, recommended.Defaults.Log)
			return editLogSetup(reader, output, &config.Defaults.Log)
		}},
		{"top", func() error {
			config.Defaults.Top = mergeConfigSection(config.Defaults.Top, recommended.Defaults.Top)
			return editTopSetup(reader, output, &config.Defaults.Top)
		}},
		{"info", func() error {
			config.Defaults.Info = mergeConfigSection(config.Defaults.Info, recommended.Defaults.Info)
			return editInfoSetup(reader, output, &config.Defaults.Info)
		}},
		{"where", func() error {
			config.Defaults.Where = mergeConfigSection(config.Defaults.Where, recommended.Defaults.Where)
			return editWhereSetup(reader, output, &config.Defaults.Where)
		}},
		{"doctor", func() error {
			config.Defaults.Doctor = mergeConfigSection(config.Defaults.Doctor, recommended.Defaults.Doctor)
			return editDoctorSetup(reader, output, &config.Defaults.Doctor)
		}},
		{"tls", func() error {
			config.Defaults.TLS = mergeConfigSection(config.Defaults.TLS, recommended.Defaults.TLS)
			return editTLSSetup(reader, output, &config.Defaults.TLS)
		}},
		{"http", func() error {
			config.Defaults.HTTP = mergeConfigSection(config.Defaults.HTTP, recommended.Defaults.HTTP)
			return editHTTPSetup(reader, output, &config.Defaults.HTTP)
		}},
		{"capture", func() error {
			config.Defaults.Capture = mergeConfigSection(config.Defaults.Capture, recommended.Defaults.Capture)
			return editCaptureSetup(reader, output, &config.Defaults.Capture)
		}},
		{"remote", func() error {
			config.Defaults.Remote = mergeConfigSection(config.Defaults.Remote, recommended.Defaults.Remote)
			return editRemoteSetup(reader, output, &config.Defaults.Remote)
		}},
		{"update", func() error {
			config.Defaults.Update = mergeConfigSection(config.Defaults.Update, recommended.Defaults.Update)
			return editUpdateSetup(reader, output, &config.Defaults.Update)
		}},
	}
	for _, section := range sections {
		configure, err := setupYesNo(reader, output, T("cli.setup.section."+section.key), false)
		if err != nil {
			return setupInputError(stderr, err)
		}
		if configure {
			if err := section.edit(); err != nil {
				return setupInputError(stderr, err)
			}
		}
	}
	if err := validateConfig(config); err != nil {
		fmt.Fprintln(stderr, T("cli.config.invalid", err))
		return 2
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	fmt.Fprintln(output, T("cli.setup.preview", path))
	fmt.Fprintln(output, "---")
	fmt.Fprint(output, string(data))
	fmt.Fprintln(output, "---")
	confirmed, err := setupYesNo(reader, output, T("cli.setup.save"), false)
	if err != nil {
		return setupInputError(stderr, err)
	}
	if !confirmed {
		fmt.Fprintln(stderr, T("cli.setup.cancelled"))
		return 4
	}
	if err := writeConfigIfUnchanged(path, data, originalSnapshot); err != nil {
		if errors.Is(err, errConfigChanged) {
			fmt.Fprintln(stderr, T("cli.setup.config_changed"))
			return 2
		}
		fmt.Fprintln(stderr, T("cli.setup.write_failed", err))
		return 2
	}
	fmt.Fprintln(output, T("cli.setup.saved", path))
	return 0
}

func setupInputError(stderr io.Writer, err error) int {
	if errors.Is(err, errSetupCancelled) {
		fmt.Fprintln(stderr, T("cli.setup.cancelled"))
		return 4
	}
	fmt.Fprintln(stderr, T("cli.setup.input_failed", err))
	return 2
}

func setupValue(reader *bufio.Reader, output io.Writer, label, current string, validate func(string) error) (string, error) {
	for {
		fmt.Fprintf(output, "%s [%s]: ", label, current)
		line, err := reader.ReadString('\n')
		if errors.Is(err, io.EOF) && line == "" {
			return "", errSetupCancelled
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		value := strings.TrimSpace(line)
		if value == "" {
			value = current
		}
		if validationErr := validate(value); validationErr != nil {
			fmt.Fprintln(output, T("cli.setup.invalid_value", validationErr))
			if errors.Is(err, io.EOF) {
				return "", io.ErrUnexpectedEOF
			}
			continue
		}
		return value, nil
	}
}

func setupYesNo(reader *bufio.Reader, output io.Writer, label string, defaultValue bool) (bool, error) {
	defaultLabel := "N"
	if defaultValue {
		defaultLabel = "Y"
	}
	value, err := setupValue(reader, output, label+" (y/n)", defaultLabel, func(value string) error {
		switch strings.ToLower(value) {
		case "y", "yes", "n", "no":
			return nil
		}
		return errors.New(T("cli.setup.validation.yes_no"))
	})
	if err != nil {
		return false, err
	}
	return strings.HasPrefix(strings.ToLower(value), "y"), nil
}

func setupString(reader *bufio.Reader, output io.Writer, label string, target **string, validate func(string) error) error {
	return setupOptionalString(reader, output, label, target, validate)
}

func setupOptionalString(reader *bufio.Reader, output io.Writer, label string, target **string, validate func(string) error) error {
	current := ""
	if *target != nil {
		current = **target
	}
	for {
		fmt.Fprintf(output, "%s [%s] (%s): ", label, current, T("cli.setup.clear_hint"))
		line, err := reader.ReadString('\n')
		if errors.Is(err, io.EOF) && line == "" {
			return errSetupCancelled
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		value := strings.TrimSpace(line)
		if value == "" {
			return nil
		}
		if value == "!clear" {
			*target = nil
			return nil
		}
		if validationErr := validate(value); validationErr != nil {
			fmt.Fprintln(output, T("cli.setup.invalid_value", validationErr))
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			continue
		}
		*target = stringPointer(value)
		return nil
	}
}
func setupDurationValue(reader *bufio.Reader, output io.Writer, label string, target **configDuration, validate func(time.Duration) error) error {
	current := ""
	if *target != nil {
		current = (*target).Duration.String()
	}
	value, clear, err := setupScalarValue(reader, output, label, current, func(value string) error {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		return validate(duration)
	})
	if err != nil {
		return err
	}
	if clear {
		*target = nil
		return nil
	}
	duration, _ := time.ParseDuration(value)
	*target = durationPointer(duration)
	return nil
}
func setupIntValue(reader *bufio.Reader, output io.Writer, label string, target **int, validate func(int) error) error {
	current := "0"
	if *target != nil {
		current = strconv.Itoa(**target)
	}
	value, clear, err := setupScalarValue(reader, output, label, current, func(value string) error {
		number, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		return validate(number)
	})
	if err != nil {
		return err
	}
	if clear {
		*target = nil
		return nil
	}
	number, _ := strconv.Atoi(value)
	*target = intPointer(number)
	return nil
}
func setupBoolValue(reader *bufio.Reader, output io.Writer, label string, target **bool) error {
	current := "false"
	if *target != nil {
		current = strconv.FormatBool(**target)
	}
	value, clear, err := setupScalarValue(reader, output, label, current, func(value string) error { _, err := strconv.ParseBool(value); return err })
	if err != nil {
		return err
	}
	if clear {
		*target = nil
		return nil
	}
	boolean, _ := strconv.ParseBool(value)
	*target = boolPointer(boolean)
	return nil
}

func setupScalarValue(reader *bufio.Reader, output io.Writer, label, current string, validate func(string) error) (string, bool, error) {
	for {
		fmt.Fprintf(output, "%s [%s] (%s): ", label, current, T("cli.setup.clear_hint"))
		line, err := reader.ReadString('\n')
		if errors.Is(err, io.EOF) && line == "" {
			return "", false, errSetupCancelled
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return "", false, err
		}
		value := strings.TrimSpace(line)
		if value == "" {
			return current, false, nil
		}
		if value == "!clear" {
			return "", true, nil
		}
		if validationErr := validate(value); validationErr != nil {
			fmt.Fprintln(output, T("cli.setup.invalid_value", validationErr))
			if errors.Is(err, io.EOF) {
				return "", false, io.ErrUnexpectedEOF
			}
			continue
		}
		return value, false, nil
	}
}
func noValidation(string) error { return nil }
func positiveDuration(value time.Duration) error {
	if value <= 0 {
		return errors.New(T("cli.setup.validation.positive"))
	}
	return nil
}
func nonNegative(value int) error {
	if value < 0 {
		return errors.New(T("cli.setup.validation.non_negative"))
	}
	return nil
}

func editCommonSetup(reader *bufio.Reader, output io.Writer, config *commonConfig) error {
	if err := setupDurationValue(reader, output, T("cli.setup.field.common_timeout"), &config.Timeout, positiveDuration); err != nil {
		return err
	}
	if err := setupOptionalString(reader, output, T("cli.setup.field.common_json"), &config.JSON, outputPathValidation(true)); err != nil {
		return err
	}
	if err := setupBoolValue(reader, output, T("cli.setup.field.common_verbose"), &config.Verbose); err != nil {
		return err
	}
	return setupBoolValue(reader, output, T("cli.setup.field.common_redact"), &config.Redact)
}
func editLogSetup(reader *bufio.Reader, output io.Writer, config *logConfig) error {
	if err := setupOptionalString(reader, output, T("cli.setup.field.log_stream"), &config.Stream, func(value string) error {
		if value != "stdout" && value != "stderr" {
			return errors.New(T("cli.setup.validation.stream"))
		}
		return nil
	}); err != nil {
		return err
	}
	if err := setupOptionalString(reader, output, T("cli.setup.field.log_output"), &config.Output, func(value string) error {
		if value == "" || value == "-" || !filepath.IsAbs(value) {
			return errors.New(T("cli.setup.validation.file_path"))
		}
		return nil
	}); err != nil {
		return err
	}
	return setupOptionalString(reader, output, T("cli.setup.field.log_command_display"), &config.CommandDisplay, func(value string) error {
		if value != "full" && value != "name" && value != "none" {
			return errors.New(T("cli.setup.validation.command_display"))
		}
		return nil
	})
}
func editTopSetup(reader *bufio.Reader, output io.Writer, config *topConfig) error {
	if err := setupDurationValue(reader, output, T("cli.setup.field.top_interval"), &config.Interval, func(value time.Duration) error {
		if value < topMinInterval {
			return errors.New(T("cli.setup.validation.top_interval"))
		}
		return nil
	}); err != nil {
		return err
	}
	if err := setupIntValue(reader, output, T("cli.setup.field.top_count"), &config.Count, nonNegative); err != nil {
		return err
	}
	if err := setupBoolValue(reader, output, T("cli.setup.field.top_no_header"), &config.NoHeader); err != nil {
		return err
	}
	return setupOptionalString(reader, output, T("cli.setup.field.top_json"), &config.JSON, outputPathValidation(true))
}
func editInfoSetup(reader *bufio.Reader, output io.Writer, config *infoConfig) error {
	if err := setupBoolValue(reader, output, T("cli.setup.field.info_public"), &config.Public); err != nil {
		return err
	}
	if err := setupDurationValue(reader, output, T("cli.setup.field.info_timeout"), &config.Timeout, positiveDuration); err != nil {
		return err
	}
	return setupBoolValue(reader, output, T("cli.setup.field.info_verbose"), &config.Verbose)
}
func editWhereSetup(reader *bufio.Reader, output io.Writer, config *whereConfig) error {
	if err := setupString(reader, output, T("cli.setup.field.where_provider"), &config.Provider, func(value string) error {
		if value != "all" && value != "aws" && value != "gcp" {
			return errors.New(T("cli.setup.validation.provider"))
		}
		return nil
	}); err != nil {
		return err
	}
	return setupIntValue(reader, output, T("cli.setup.field.where_count"), &config.Count, func(value int) error {
		if value < 1 {
			return errors.New(T("cli.setup.validation.positive"))
		}
		return nil
	})
}
func editDoctorSetup(reader *bufio.Reader, output io.Writer, config *doctorConfig) error {
	return setupString(reader, output, T("cli.setup.field.doctor_profile"), &config.Profile, func(value string) error {
		if value != "default" && value != "full" {
			return errors.New(T("cli.setup.validation.profile"))
		}
		return nil
	})
}
func editTLSSetup(reader *bufio.Reader, output io.Writer, config *tlsConfig) error {
	return setupIntValue(reader, output, T("cli.setup.field.tls_min_days"), &config.MinDays, nonNegative)
}
func editHTTPSetup(reader *bufio.Reader, output io.Writer, config *httpConfig) error {
	return setupIntValue(reader, output, T("cli.setup.field.http_expect_status"), &config.ExpectStatus, func(value int) error {
		if value != 0 && (value < 100 || value > 599) {
			return errors.New(T("cli.setup.validation.http_status"))
		}
		return nil
	})
}
func editCaptureSetup(reader *bufio.Reader, output io.Writer, config *captureConfig) error {
	if err := setupOptionalString(reader, output, T("cli.setup.field.capture_interface"), &config.Interface, noValidation); err != nil {
		return err
	}
	if err := setupDurationValue(reader, output, T("cli.setup.field.capture_duration"), &config.Duration, func(value time.Duration) error {
		if value <= 0 || value > maxCaptureDuration {
			return errors.New(T("cli.setup.validation.capture_duration"))
		}
		return nil
	}); err != nil {
		return err
	}
	if err := setupIntValue(reader, output, T("cli.setup.field.capture_count"), &config.Count, func(value int) error {
		if value <= 0 || value > maxCapturePackets {
			return errors.New(T("cli.setup.validation.capture_count"))
		}
		return nil
	}); err != nil {
		return err
	}
	if err := setupOptionalString(reader, output, T("cli.setup.field.capture_filter"), &config.Filter, noValidation); err != nil {
		return err
	}
	return setupOptionalString(reader, output, T("cli.setup.field.capture_output"), &config.Output, outputPathValidation(false))
}
func editRemoteSetup(reader *bufio.Reader, output io.Writer, config *remoteConfig) error {
	absolutePath := func(value string) error {
		if value != "" && !filepath.IsAbs(value) {
			return errors.New(T("cli.setup.validation.absolute_path"))
		}
		return nil
	}
	if err := setupOptionalString(reader, output, T("cli.setup.field.remote_inventory"), &config.Inventory, absolutePath); err != nil {
		return err
	}
	if err := setupOptionalString(reader, output, T("cli.setup.field.remote_recipe"), &config.Recipe, absolutePath); err != nil {
		return err
	}
	if err := setupDurationValue(reader, output, T("cli.setup.field.remote_connect_timeout"), &config.ConnectTimeout, func(value time.Duration) error {
		if value <= 0 || value > remoteMaxTimeout {
			return errors.New(T("cli.setup.validation.remote_timeout"))
		}
		return nil
	}); err != nil {
		return err
	}
	if err := setupIntValue(reader, output, T("cli.setup.field.remote_output_limit"), &config.OutputLimit, func(value int) error {
		if value <= 0 || value > remoteConfigLimit {
			return errors.New(T("cli.setup.validation.remote_output_limit"))
		}
		return nil
	}); err != nil {
		return err
	}
	if err := setupIntValue(reader, output, T("cli.setup.field.remote_parallel"), &config.Parallel, func(value int) error {
		if value < 0 || value > remoteHostLimit {
			return errors.New(T("cli.setup.validation.remote_parallel"))
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}
func editUpdateSetup(reader *bufio.Reader, output io.Writer, config *updateConfig) error {
	return setupDurationValue(reader, output, T("cli.setup.field.update_timeout"), &config.Timeout, positiveDuration)
}

func outputPathValidation(allowStdout bool) func(string) error {
	return func(value string) error {
		if value == "" || (allowStdout && value == "-") || filepath.IsAbs(value) {
			return nil
		}
		return errors.New(T("cli.setup.validation.absolute_path"))
	}
}

func writeConfigAtomic(path string, data []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".config-*.yaml")
	if err != nil {
		return err
	}
	temporary := file.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	directoryFile, err := os.Open(directory)
	if err != nil {
		return err
	}
	if err := directoryFile.Sync(); err != nil {
		directoryFile.Close()
		return err
	}
	if err := directoryFile.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}

type configSnapshot struct {
	exists  bool
	mode    os.FileMode
	size    int64
	modTime int64
	hash    [sha256.Size]byte
	hashed  bool
}

func configFileSnapshot(path string) (configSnapshot, error) {
	var snapshot configSnapshot
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, err
	}
	snapshot.exists = true
	snapshot.mode = info.Mode()
	snapshot.size = info.Size()
	snapshot.modTime = info.ModTime().UnixNano()
	if !info.Mode().IsRegular() || info.Size() > maxConfigBytes {
		return snapshot, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return configSnapshot{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return configSnapshot{}, err
	}
	if len(data) > maxConfigBytes {
		return snapshot, nil
	}
	snapshot.hash = sha256.Sum256(data)
	snapshot.hashed = true
	return snapshot, nil
}

var errConfigChanged = errors.New("config changed")

func writeConfigIfUnchanged(path string, data []byte, original configSnapshot) error {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if mkdirErr := os.MkdirAll(filepath.Dir(path), 0o700); mkdirErr != nil {
				return mkdirErr
			}
			lock, err = os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
		}
		if err != nil {
			return err
		}
	}
	defer lock.Close()
	interrupted, err := lockLogFile(lock, make(chan os.Signal))
	if err != nil {
		return err
	}
	if interrupted != nil {
		return fmt.Errorf("config lock interrupted by %v", interrupted)
	}
	defer unlockLogFile(lock)
	current, err := configFileSnapshot(path)
	if err != nil {
		return err
	}
	if current != original {
		return errConfigChanged
	}
	return writeConfigAtomic(path, data)
}
