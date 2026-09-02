package edc

import (
	"bufio"
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
		fmt.Fprintln(os.Stderr, "capture는 현재 macOS만 지원합니다")
		return 2
	}
	set := flag.NewFlagSet("capture", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	interfaceName := set.String("interface", "", "capture할 network interface (필수)")
	duration := set.Duration("duration", 15*time.Second, "capture 시간 (최대 60s)")
	count := set.Int("count", 500, "packet 수 (최대 10000)")
	filter := set.String("filter", "", "BPF filter")
	output := set.String("output", "", "pcap 저장 경로")
	yes := set.Bool("yes", false, "interactive 확인 생략")
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "알 수 없는 positional argument가 있습니다. BPF expression은 --filter로 전달하세요")
		return 2
	}
	if *interfaceName == "" {
		fmt.Fprintln(os.Stderr, "--interface가 필요합니다")
		return 2
	}
	if *duration <= 0 || *duration > maxCaptureDuration {
		fmt.Fprintln(os.Stderr, "--duration은 0초보다 크고 60초 이하여야 합니다")
		return 2
	}
	if *count <= 0 || *count > maxCapturePackets {
		fmt.Fprintln(os.Stderr, "--count는 1 이상 10000 이하여야 합니다")
		return 2
	}
	if !validInterface(*interfaceName) {
		fmt.Fprintf(os.Stderr, "존재하지 않는 interface: %s\n", *interfaceName)
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
		fmt.Fprintln(os.Stderr, "capture를 취소했습니다")
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
		fmt.Fprintf(os.Stderr, "capture 실패: %v\n", err)
		return 1
	}
	if err := os.Chmod(outputPath, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "capture는 완료됐지만 권한 설정에 실패했습니다: %v\n", err)
		return 1
	}
	fmt.Printf("capture 완료: %s\n", outputPath)
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
			return "", fmt.Errorf("기존 파일을 덮어쓰지 않습니다: %s", absolute)
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

const (
	capturePayloadWarning = "주의: packet payload에는 credential이나 개인정보가 포함될 수 있습니다."
	// captureLabelWidth는 한글 label도 값 열이 어긋나지 않게 표시 폭으로 맞춘다.
	captureLabelWidth = 14
)

func (plan capturePlan) detail() string {
	rows := [][2]string{
		{"interface", plan.interfaceName},
		{"duration", plan.duration.String()},
		{"packet limit", fmt.Sprint(plan.count)},
		{"filter", emptyAs(plan.filter, "(none)")},
		{"output", plan.outputPath},
		{"권한", plan.privilegeLabel()},
	}
	var builder strings.Builder
	builder.WriteString("capture 계획\n")
	for _, row := range rows {
		builder.WriteString("  " + liveCell(row[0], captureLabelWidth) + row[1] + "\n")
	}
	builder.WriteString(capturePayloadWarning + "\n")
	return builder.String()
}

func (plan capturePlan) privilegeLabel() string {
	if plan.privileged {
		return "root로 실행합니다"
	}
	return "sudo로 tcpdump를 실행합니다"
}

// confirmCapture는 terminal에서는 확인 화면을, 그 외에는 y/N 입력을 쓴다.
func confirmCapture(input *os.File, output *os.File, plan capturePlan) bool {
	if term.IsTerminal(int(input.Fd())) {
		answer, err := runConfirmModel(input, output, newDetailedConfirmModel(plan.detail(), "capture를 실행할까요?", false))
		return err == nil && answer
	}
	fmt.Fprint(output, plan.detail())
	return confirm(input, output)
}

func confirm(input *os.File, output *os.File) bool {
	fmt.Fprint(output, "계속하시겠습니까? [y/N] ")
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
