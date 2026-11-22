package edc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListenBrowseModePrecedence(t *testing.T) {
	cases := []struct {
		name, mode            string
		override              *bool
		terminal, watch, json bool
		want, fail            bool
	}{
		{name: "terminal defaults to browser", mode: "auto", terminal: true, want: true},
		{name: "pipe defaults to text", mode: "auto"},
		{name: "never in terminal", mode: "never", terminal: true},
		{name: "always in terminal", mode: "always", terminal: true, want: true},
		{name: "always requires terminal", mode: "always", fail: true},
		{name: "b overrides never", mode: "never", override: boolPointer(true), terminal: true, want: true},
		{name: "B overrides always in pipe", mode: "always", override: boolPointer(false)},
		{name: "B overrides auto in terminal", mode: "auto", override: boolPointer(false), terminal: true},
		{name: "b requires terminal", mode: "never", override: boolPointer(true), fail: true},
		{name: "watch overrides always", mode: "always", watch: true, terminal: true},
		{name: "JSON overrides always in pipe", mode: "always", json: true},
		{name: "b conflicts with watch", mode: "auto", override: boolPointer(true), watch: true, terminal: true, fail: true},
		{name: "b conflicts with JSON", mode: "auto", override: boolPointer(true), json: true, terminal: true, fail: true},
		{name: "invalid mode", mode: "sometimes", terminal: true, fail: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, noColor := range []string{"", "1"} {
				t.Setenv("NO_COLOR", noColor)
				got, err := resolveListenBrowse(tc.mode, tc.override, tc.terminal, tc.watch, tc.json)
				if got != tc.want || (err != nil) != tc.fail {
					t.Fatalf("NO_COLOR=%s: browse=%t error=%v", noColor, got, err)
				}
			}
		})
	}
}

func TestListenBrowseConfigValidationAndSetupPersistence(t *testing.T) {
	for _, extension := range []string{"yaml", "toml"} {
		path := filepath.Join(t.TempDir(), "config."+extension)
		for _, mode := range []string{"auto", "always", "never"} {
			var out, stderr strings.Builder
			if code := runSetupSet([]string{"listen.browse", mode}, &out, &stderr, path); code != 0 {
				t.Fatalf("%s %s: %d %s", extension, mode, code, stderr.String())
			}
			config, err := loadConfigAt(path)
			if err != nil || configuredString(config.Defaults.Listen.Browse, "") != mode {
				t.Fatalf("saved %s %s: %+v %v", extension, mode, config.Defaults.Listen, err)
			}
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var out, stderr strings.Builder
		if code := runSetupSet([]string{"listen.browse", "sometimes"}, &out, &stderr, path); code != 2 {
			t.Fatalf("invalid mode exit: %d", code)
		}
		after, _ := os.ReadFile(path)
		if string(before) != string(after) {
			t.Fatal("invalid mode overwrote config")
		}
		out.Reset()
		stderr.Reset()
		if code := runSetupSet([]string{"defaults.listen.browse", "--clear"}, &out, &stderr, path); code != 0 {
			t.Fatalf("clear: %d %s", code, stderr.String())
		}
		config, err := loadConfigAt(path)
		if err != nil || config.Defaults.Listen.Browse != nil {
			t.Fatalf("clear: %+v %v", config.Defaults.Listen, err)
		}
		if mode := configuredString(config.Defaults.Listen.Browse, "auto"); mode != "auto" {
			t.Fatalf("cleared fallback: %s", mode)
		}
	}
	for _, value := range []string{"true", "42", "[auto]", "''", "sometimes"} {
		path := writeConfigFixture(t, "defaults:\n  listen:\n    browse: "+value+"\n")
		if _, err := loadConfigAt(path); err == nil {
			t.Fatalf("accepted browse %s", value)
		}
	}
}

func TestSetupSelectsListenBrowseDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	input := "en\n" + strings.Repeat("n\n", 17) + "y\nsometimes\nnever\ny\n"
	var out, stderr strings.Builder
	if code := runSetupWithIO(nil, strings.NewReader(input), &out, &stderr, true, path); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr.String(), out.String())
	}
	config, err := loadConfigAt(path)
	if err != nil || configuredString(config.Defaults.Listen.Browse, "") != "never" {
		t.Fatalf("config: %+v %v", config.Defaults.Listen, err)
	}
	for _, want := range []string{"auto", "always", "never", "[auto]"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("wizard missing %s: %s", want, out.String())
		}
	}
}

func TestListenRejectsOppositeBrowseFlags(t *testing.T) {
	for _, args := range [][]string{{"-b", "-B"}, {"--browse", "--no-browse"}} {
		if code := runListen(args, "test"); code != 2 {
			t.Fatalf("%v exit %d", args, code)
		}
	}
}
