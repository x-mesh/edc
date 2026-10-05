package edc

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	logMiB                 = 1024 * 1024
	defaultLogMaxSizeMB    = 10
	defaultLogKeepFiles    = 3
	defaultLogMaxRestarts  = 3
	defaultLogRestartDelay = 5 * time.Second
	defaultLogKillAfter    = 5 * time.Second
	logMaxSizeMB           = 1024 * 1024
	logMaxKeepFiles        = 100
	logMaxRestarts         = 1000
)

func validateLogPolicy(options logOptions) error {
	if options.maxSizeMB < 0 || options.maxSizeMB > logMaxSizeMB {
		return fmt.Errorf("--max-size: %s", T("cli.setup.validation.log_max_size"))
	}
	if options.keepFiles < 1 || options.keepFiles > logMaxKeepFiles {
		return fmt.Errorf("--keep-files: %s", T("cli.setup.validation.log_keep_files"))
	}
	if options.restart != "never" && options.restart != "on-failure" && options.restart != "always" {
		return fmt.Errorf("--restart: %s", T("cli.setup.validation.log_restart"))
	}
	if options.maxRestarts < 0 || options.maxRestarts > logMaxRestarts {
		return fmt.Errorf("--max-restarts: %s", T("cli.setup.validation.log_max_restarts"))
	}
	if options.restartDelay < 0 {
		return fmt.Errorf("--restart-delay: %s", T("cli.setup.validation.non_negative"))
	}
	if options.timeout < 0 {
		return fmt.Errorf("--timeout: %s", T("cli.setup.validation.non_negative"))
	}
	if options.killAfter <= 0 {
		return fmt.Errorf("--kill-after: %s", T("cli.setup.validation.positive"))
	}
	return nil
}

func defaultLogDirectory() string {
	path := recommendedLogOutputPath()
	if path == "" {
		return ""
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(filepath.Dir(path), "edc")
	}
	return filepath.Join(filepath.Dir(path), "log")
}

func createDefaultLogOutput(command, root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("the default log directory is unavailable; specify --output")
	}
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, filepath.Base(command))
	if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
		return "", err
	}
	prefix := time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + strconv.Itoa(os.Getpid()) + "-"
	file, err := os.CreateTemp(filepath.Join(root, name), prefix+"*.log")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		return "", err
	}
	return path, nil
}

type rotatingLogFile struct {
	file          *os.File
	path          string
	size, maxSize int64
	keep          int
	mode          os.FileMode
	failures      chan error
}

func openRotatingLog(path string, maxSize int64, keep int) (*rotatingLogFile, error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("log path must be a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	file, err := openLogFile(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	return &rotatingLogFile{file: file, path: path, size: info.Size(), maxSize: maxSize, keep: keep, mode: info.Mode().Perm(), failures: make(chan error, 1)}, nil
}

func (file *rotatingLogFile) Close() error { return file.file.Close() }
func (file *rotatingLogFile) Sync() error  { return file.file.Sync() }
func (file *rotatingLogFile) archive(index int) string {
	return file.path + ".edc." + strconv.Itoa(index)
}

func (file *rotatingLogFile) rotate() error {
	for index := 1; index <= file.keep; index++ {
		info, err := os.Lstat(file.archive(index))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("rotation archive must be a regular file: %s", file.archive(index))
		}
	}
	if err := file.file.Sync(); err != nil {
		return err
	}
	if err := file.file.Close(); err != nil {
		return err
	}
	if err := os.Remove(file.archive(file.keep)); err != nil && !os.IsNotExist(err) {
		return err
	}
	for index := file.keep - 1; index >= 1; index-- {
		if err := os.Rename(file.archive(index), file.archive(index+1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(file.path, file.archive(1)); err != nil {
		return err
	}
	opened, err := openLogFile(file.path)
	if err != nil {
		return err
	}
	file.file = opened
	if err := opened.Chmod(file.mode); err != nil {
		return err
	}
	file.size = 0
	return nil
}

func (file *rotatingLogFile) Write(data []byte) (int, error) {
	written := 0
	if file.maxSize > 0 && int64(len(data)) <= file.maxSize && file.size+int64(len(data)) > file.maxSize {
		if err := file.rotate(); err != nil {
			file.fail(err)
			return 0, err
		}
	}
	for len(data) > 0 {
		if file.maxSize > 0 && file.size >= file.maxSize {
			if err := file.rotate(); err != nil {
				file.fail(err)
				return written, err
			}
		}
		part := len(data)
		if file.maxSize > 0 && int64(part) > file.maxSize-file.size {
			part = int(file.maxSize - file.size)
		}
		n, err := file.file.Write(data[:part])
		written += n
		file.size += int64(n)
		data = data[n:]
		if err == nil && n != part {
			err = io.ErrShortWrite
		}
		if err != nil {
			file.fail(err)
			return written, err
		}
	}
	return written, nil
}

func (file *rotatingLogFile) fail(err error) {
	select {
	case file.failures <- err:
	default:
	}
}
