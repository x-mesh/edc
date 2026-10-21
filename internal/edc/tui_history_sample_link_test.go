package edc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHistoryTopProcessLinkUsesSelectedSampleID(t *testing.T) {
	source := "/private/tmp/claude-501/-Users-jinwoo-work-project-agentic-edc/7ff0ceb5-9043-4d96-84e6-dddf5193bda1/scratchpad/h.db"
	data, err := os.ReadFile(source)
	if err != nil {
		t.Skipf("provided history DB unavailable: %v", err)
	}
	path := filepath.Join(t.TempDir(), "history.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	samples, err := loadHistoryTop(db, historyOptions{run: "e07790782a1cca08"})
	if err != nil || len(samples) == 0 {
		t.Fatalf("top samples=%d err=%v", len(samples), err)
	}
	processes, err := loadHistoryProcessSample(db, samples[0].RunID, samples[0].sampleID)
	if err != nil || len(processes) == 0 {
		t.Fatalf("sample id=%d run=%q processes=%d err=%v", samples[0].sampleID, samples[0].RunID, len(processes), err)
	}
}
