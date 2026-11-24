package edc

import (
	"fmt"
	"io"
)

type observeHelpTopic struct {
	name     string
	commands []string
}

type observeHelpSection struct {
	name    string
	members []commandDoc
}

var observeHelpTopics = []observeHelpTopic{
	{"resources", []string{"top", "info", "du", "clean", "ai"}},
	{"processes", []string{"proc", "svc", "jobs", "ctr"}},
	{"logs_files", []string{"view", "cat", "grep", "find", "search", "watch"}},
	{"network", []string{"where", "listen", "quality", "capture"}},
	{"tracing_changes", []string{"trace", "changes"}},
}

func observeHelpSections(members []commandDoc) []observeHelpSection {
	byName := make(map[string]commandDoc, len(members))
	for _, doc := range members {
		if doc.group == "observe" {
			byName[doc.name] = doc
		}
	}
	seen := make(map[string]bool, len(members))
	var sections []observeHelpSection
	for _, topic := range observeHelpTopics {
		section := observeHelpSection{name: topic.name}
		for _, name := range topic.commands {
			if doc, exists := byName[name]; exists && !seen[name] {
				section.members = append(section.members, doc)
				seen[name] = true
			}
		}
		if len(section.members) > 0 {
			sections = append(sections, section)
		}
	}
	other := observeHelpSection{name: "other"}
	for _, doc := range members {
		if doc.group == "observe" && !seen[doc.name] {
			other.members = append(other.members, doc)
			seen[doc.name] = true
		}
	}
	if len(other.members) > 0 {
		sections = append(sections, other)
	}
	return sections
}

func printObserveHelp(writer io.Writer, members []commandDoc, width int, color bool) {
	for _, section := range observeHelpSections(members) {
		heading := T("helptopics."+section.name) + ":"
		if color {
			fmt.Fprintln(writer, richPanelLine(richDim(heading), ""))
		} else {
			fmt.Fprintln(writer, "  "+heading)
		}
		for _, doc := range section.members {
			if color {
				fmt.Fprintln(writer, richPanelLine(richPaint(padRight(doc.name, width), richCyan, true)+"  "+doc.summary(), ""))
			} else {
				fmt.Fprintf(writer, "  %-*s  %s\n", width, doc.name, doc.summary())
			}
		}
	}
}
