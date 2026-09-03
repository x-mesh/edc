package edc

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"
)

const (
	maxCaptureDuration = 60 * time.Second
	maxCapturePackets  = 10_000
)

func runCapture(args []string) int {
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(os.Stderr, T("cli.capture.macos_only"))
		return 2
	}
	set := flag.NewFlagSet("capture", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	config := activeConfig.Defaults.Capture
	interfaceName := set.String("interface", configuredString(config.Interface, ""), T("command.capture.option.interface"))
	duration := set.Duration("duration", configuredDuration(config.Duration, 15*time.Second), T("command.capture.option.duration"))
	count := set.Int("count", configuredInt(config.Count, 500), T("command.capture.option.count"))
	filter := set.String("filter", configuredString(config.Filter, ""), T("command.capture.option.filter"))
	output := set.String("output", configuredString(config.Output, ""), T("command.capture.option.output"))
	yes := set.Bool("yes", false, T("command.capture.option.yes"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.capture.unexpected_positional"))
		return 2
	}
	if *interfaceName == "" {
		fmt.Fprintln(os.Stderr, T("cli.capture.interface_required"))
		return 2
	}
	if *duration <= 0 || *duration > maxCaptureDuration {
		fmt.Fprintln(os.Stderr, T("cli.capture.duration_range"))
		return 2
	}
	if *count <= 0 || *count > maxCapturePackets {
		fmt.Fprintln(os.Stderr, T("cli.capture.count_range"))
		return 2
	}
	if !validInterface(*interfaceName) {
		fmt.Fprintln(os.Stderr, T("cli.capture.unknown_interface", *interfaceName))
		return 2
	}

	outputPath, err := captureOutputPath(*output)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	plan := capturePlan{
		interfaceName: *interfaceName, duration: *duration, count: *count,
		filter: *filter, outputPath: outputPath, privileged: os.Geteuid() == 0,
	}
	if !*yes && !confirmCapture(os.Stdin, os.Stdout, plan) {
		fmt.Fprintln(os.Stderr, T("cli.capture.cancelled"))
		return 4
	}

	tcpdumpArgs := []string{"-i", *interfaceName, "-n", "-U", "-c", fmt.Sprint(*count), "-G", fmt.Sprint(int(duration.Seconds())), "-W", "1", "-w", outputPath}
	if *filter != "" {
		tcpdumpArgs = append(tcpdumpArgs, strings.Fields(*filter)...)
	}
	commandPath := "/usr/sbin/tcpdump"
	commandArgs := tcpdumpArgs
	if os.Geteuid() != 0 {
		commandPath = "/usr/bin/sudo"
		commandArgs = append([]string{"/usr/sbin/tcpdump"}, tcpdumpArgs...)
	}
	command := exec.Command(commandPath, commandArgs...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
			return 3
		}
		fmt.Fprintln(os.Stderr, T("cli.capture.failed", err))
		return 1
	}
	if err := os.Chmod(outputPath, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, T("cli.capture.chmod_failed", err))
		return 1
	}
	fmt.Println(T("cli.capture.done", outputPath))
	return 0
}

func validInterface(name string) bool {
	interfaces, err := netInterfaces()
	if err != nil {
		return false
	}
	for _, candidate := range interfaces {
		if candidate == name {
			return true
		}
	}
	return false
}

func netInterfaces() ([]string, error) {
	interfaces, err := netInterfaceList()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(interfaces))
	for _, iface := range interfaces {
		names = append(names, iface.Name)
	}
	return names, nil
}

var netInterfaceList = net.Interfaces

func captureOutputPath(requested string) (string, error) {
	if requested != "" {
		absolute, err := filepath.Abs(requested)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(absolute); err == nil {
			return "", errors.New(T("cli.capture.output_exists", absolute))
		} else if !os.IsNotExist(err) {
			return "", err
		}
		return absolute, nil
	}
	file, err := os.CreateTemp("", "edc-capture-*.pcap")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

// capturePlan은 확인 화면이 보여 주는 실행 조건이다.
type capturePlan struct {
	interfaceName string
	duration      time.Duration
	count         int
	filter        string
	outputPath    string
	privileged    bool // 이미 root라 sudo를 쓰지 않는다
}

// captureLabelWidth는 한글 label도 값 열이 어긋나지 않게 표시 폭으로 맞춘다.
const captureLabelWidth = 14

// capturePayloadWarning은 payload가 무엇을 담을 수 있는지 알리는 경고다.
func capturePayloadWarning() string { return T("cli.capture.payload_warning") }

func (plan capturePlan) detail() string {
	rows := [][2]string{
		{"interface", plan.interfaceName},
		{"duration", plan.duration.String()},
		{"packet limit", fmt.Sprint(plan.count)},
		{"filter", emptyAs(plan.filter, "(none)")},
		{"output", plan.outputPath},
		{T("cli.capture.label.privilege"), plan.privilegeLabel()},
	}
	var builder strings.Builder
	builder.WriteString(T("cli.capture.plan_title") + "\n")
	for _, row := range rows {
		builder.WriteString("  " + liveCell(row[0], captureLabelWidth) + row[1] + "\n")
	}
	builder.WriteString(capturePayloadWarning() + "\n")
	return builder.String()
}

func (plan capturePlan) privilegeLabel() string {
	if plan.privileged {
		return T("cli.capture.privilege_root")
	}
	return T("cli.capture.privilege_sudo")
}

// confirmCapture는 terminal에서는 확인 화면을, 그 외에는 y/N 입력을 쓴다.
func confirmCapture(input *os.File, output *os.File, plan capturePlan) bool {
	if term.IsTerminal(int(input.Fd())) {
		answer, err := runConfirmModel(input, output, newDetailedConfirmModel(plan.detail(), T("cli.capture.confirm"), false))
		return err == nil && answer
	}
	fmt.Fprint(output, plan.detail())
	return confirm(input, output)
}

func confirm(input *os.File, output *os.File) bool {
	fmt.Fprint(output, T("cli.confirm"))
	line, err := bufio.NewReader(input).ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

func emptyAs(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
