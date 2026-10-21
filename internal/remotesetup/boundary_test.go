package remotesetup

import (
	"os/exec"
	"strings"
	"testing"
)

// The core must stay portable: it may import the standard library only, and the tui adapter
// must not import edc. A consumer that copies this package must not need anything else.
func TestPackagesKeepTheirImportBoundary(t *testing.T) {
	list := func(pattern string) []string {
		output, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", pattern).Output()
		if err != nil {
			t.Fatalf("go list %s: %v", pattern, err)
		}
		return strings.Fields(string(output))
	}
	for _, path := range list(".") {
		if path != "github.com/x-mesh/edc/internal/remotesetup" {
			t.Errorf("the core imports %s", path)
		}
	}
	for _, path := range list("./tui") {
		if strings.HasPrefix(path, "github.com/x-mesh/edc/") && path != "github.com/x-mesh/edc/internal/remotesetup" && path != "github.com/x-mesh/edc/internal/remotesetup/tui" {
			t.Errorf("the tui adapter imports %s", path)
		}
	}
}
