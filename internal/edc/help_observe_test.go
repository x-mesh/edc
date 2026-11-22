package edc

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestObserveHelpSectionsCoverEveryCommandOnce(t *testing.T) {
	expected := []observeHelpTopic{
		{"resources", []string{"top", "info", "du", "clean", "ai"}},
		{"processes", []string{"proc", "svc", "jobs", "ctr"}},
		{"logs_files", []string{"view", "grep", "find", "search", "watch"}},
		{"network", []string{"where", "listen", "quality", "capture"}},
		{"tracing_changes", []string{"trace", "changes"}},
	}
	if !reflect.DeepEqual(observeHelpTopics, expected) {
		t.Fatalf("topic membership or order changed: %+v", observeHelpTopics)
	}
	members, _ := helpGroupMembers("observe")
	sections := observeHelpSections(members)
	if len(sections) != len(expected) {
		t.Fatalf("sections: %+v", sections)
	}
	docs := make(map[string]commandDoc, len(members))
	for _, doc := range members {
		docs[doc.name] = doc
	}
	seen := make(map[string]int, len(members))
	for index, section := range sections {
		if section.name != expected[index].name {
			t.Fatalf("section %d: %s", index, section.name)
		}
		var names []string
		for _, doc := range section.members {
			original, ok := docs[doc.name]
			if !ok || doc.group != "observe" || !reflect.DeepEqual(doc, original) {
				t.Fatalf("section includes changed or non-observe command: %+v", doc)
			}
			names = append(names, doc.name)
			seen[doc.name]++
		}
		if !reflect.DeepEqual(names, expected[index].commands) {
			t.Fatalf("%s members: %v", section.name, names)
		}
	}
	for _, doc := range members {
		if seen[doc.name] != 1 {
			t.Fatalf("%s rendered %d times", doc.name, seen[doc.name])
		}
	}
	if len(seen) != len(members) {
		t.Fatalf("rendered %d commands, expected %d", len(seen), len(members))
	}
}

func TestObserveHelpFutureCommandUsesOther(t *testing.T) {
	members, _ := helpGroupMembers("observe")
	first := commandDoc{name: "future-first", group: "observe", usage: []string{"edc future-first"}}
	second := commandDoc{name: "future-second", group: "observe"}
	input := append(append([]commandDoc{}, members...), first, first, second, commandDoc{name: "doctor", group: "diagnose"})
	sections := observeHelpSections(input)
	other := sections[len(sections)-1]
	if other.name != "other" || !reflect.DeepEqual(other.members, []commandDoc{first, second}) {
		t.Fatalf("fallback: %+v", other)
	}
	seen := map[string]int{}
	for _, section := range sections {
		if len(section.members) == 0 {
			t.Fatalf("empty section: %s", section.name)
		}
		for _, doc := range section.members {
			seen[doc.name]++
		}
	}
	if seen[first.name] != 1 || seen[second.name] != 1 || seen["doctor"] != 0 {
		t.Fatalf("fallback duplicated or mixed commands: %v", seen)
	}
	if sections := observeHelpSections(nil); len(sections) != 0 {
		t.Fatalf("empty input produced sections: %+v", sections)
	}
}

func TestObserveHelpPlainAndRichHaveMatchingTopics(t *testing.T) {
	previous := currentLanguage()
	defer setLanguage(previous)
	members, _ := helpGroupMembers("observe")
	for _, language := range supportedLanguages {
		setLanguage(language)
		var plain, rich strings.Builder
		printHelp(&plain, "test")
		printRichHelp(&rich, "test")
		richText := ansi.Strip(rich.String())
		previousPlain, previousRich := -1, -1
		for _, topic := range observeHelpTopics {
			heading := T("helptopics."+topic.name) + ":"
			if strings.HasPrefix(heading, "helptopics.") {
				t.Fatalf("%s misses %s", language, topic.name)
			}
			plainIndex, richIndex := strings.Index(plain.String(), "  "+heading+"\n"), strings.Index(richText, "│ "+heading+"\n")
			if plainIndex <= previousPlain || richIndex <= previousRich || strings.Count(plain.String(), heading) != 1 || strings.Count(richText, heading) != 1 {
				t.Fatalf("%s subsection missing, duplicated or reordered: %s", language, heading)
			}
			previousPlain, previousRich = plainIndex, richIndex
		}
		if strings.Contains(plain.String(), T("helptopics.other")+":") || strings.Contains(richText, T("helptopics.other")+":") {
			t.Fatalf("%s rendered empty Other section", language)
		}
		for _, doc := range members {
			if strings.Count(plain.String(), "  "+doc.name+" ") != 1 || strings.Count(richText, "│ "+doc.name+" ") != 1 {
				t.Fatalf("%s command not rendered once in both outputs: %s", language, doc.name)
			}
		}
		assertWidth(t, language+" observe plain help", plain.String())
		assertWidth(t, language+" observe rich help", rich.String())
	}
}
