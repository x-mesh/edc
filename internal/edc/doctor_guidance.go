package edc

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

const doctorGuidanceExcerptLimit = 160

func printDoctorGuidance(writer io.Writer, report Report, color bool) {
	style := newTextStyle(color)
	stages := []struct{ probe, command string }{
		{"dns.lookup", "edc dns lookup 'HOST'"},
		{"tcp.check", "edc tcp check 'HOST:PORT'"},
		{"tls.check", "edc tls check 'HOST:PORT'"},
		{"http.check", "edc http check 'URL'"},
	}
	heading := false
	for _, stage := range stages {
		for _, result := range report.Results {
			if result.Probe != stage.probe || (result.Status != StatusFail && result.Status != StatusWarn) {
				continue
			}
			if !heading {
				fmt.Fprintln(writer)
				style.line(writer, style.heading(T("observe.doctor.guidance_heading")))
				style.line(writer, style.dim(T("observe.doctor.guidance_manual")))
				heading = true
			}
			kind := ""
			if result.Error != nil && result.Error.Kind != "" {
				kind = " · " + doctorGuidanceExcerpt(result.Error.Kind)
			}
			excerpt := result.Summary
			if len(result.Evidence) > 0 {
				evidence := result.Evidence[0]
				excerpt += " · " + evidence.Label + ": " + evidence.Value
			}
			style.line(writer, doctorGuidanceStage(style, stage.probe, result.Status, kind))
			fmt.Fprintln(writer, doctorGuidanceEvidence(style, doctorGuidanceExcerpt(excerpt)))
			style.line(writer, "  "+style.command(stage.command))
			break
		}
	}
}

// doctorGuidanceStage는 "probe [status] · kind"다. 색을 쓰면 상태를 그 상태 색으로, kind를 회색으로 칠한다.
func doctorGuidanceStage(style textStyle, probe string, status Status, kind string) string {
	if !style.color {
		return T("observe.doctor.guidance_stage", probe, string(status), kind)
	}
	return strings.NewReplacer("\x00", richBold(probe), "\x01", style.paint(string(status), richStatusColor(status)), "\x02", style.dim(kind)).
		Replace(T("observe.doctor.guidance_stage", "\x00", "\x01", "\x02"))
}

// doctorGuidanceEvidence는 관측 근거 줄이다. 색을 쓰면 잘라 버리지 않고 단어 단위로 접어 근거를 모두 남긴다.
func doctorGuidanceEvidence(style textStyle, excerpt string) string {
	if !style.color {
		return T("observe.doctor.guidance_evidence", excerpt)
	}
	before, after, _ := strings.Cut(T("observe.doctor.guidance_evidence", "\x00"), "\x00")
	words := strings.Split(excerpt, " ")
	words[0] = style.dim(before) + words[0]
	words[len(words)-1] += style.dim(after)
	return style.wrap(words, " ", "    ")
}

func doctorGuidanceExcerpt(value string) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, ansi.Strip(value))
	clean = strings.Join(strings.Fields(clean), " ")
	runes := []rune(clean)
	if len(runes) > doctorGuidanceExcerptLimit {
		return string(runes[:doctorGuidanceExcerptLimit-1]) + "…"
	}
	return clean
}

func emitDoctor(options commonOptions, report Report) int {
	code := emit(options, report)
	if options.jsonPath == "" {
		printDoctorGuidance(os.Stdout, report, richEnabled(os.Stdout))
	}
	return code
}

func printDoctorTail(writer io.Writer, report Report, verbose, color bool) {
	printResultTail(writer, report.Results, verbose, color)
	printDoctorGuidance(writer, report, color)
}
