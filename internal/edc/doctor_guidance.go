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

func printDoctorGuidance(writer io.Writer, report Report) {
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
				fmt.Fprintln(writer, "\n"+T("observe.doctor.guidance_heading"))
				fmt.Fprintln(writer, T("observe.doctor.guidance_manual"))
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
			fmt.Fprintln(writer, T("observe.doctor.guidance_stage", stage.probe, string(result.Status), kind))
			fmt.Fprintln(writer, T("observe.doctor.guidance_evidence", doctorGuidanceExcerpt(excerpt)))
			fmt.Fprintln(writer, "  "+stage.command)
			break
		}
	}
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
		printDoctorGuidance(os.Stdout, report)
	}
	return code
}

func printDoctorTail(writer io.Writer, report Report, verbose, color bool) {
	printResultTail(writer, report.Results, verbose, color)
	printDoctorGuidance(writer, report)
}
