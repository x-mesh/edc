package edc

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// logHistoryDeletion deletes whole log families only. A family is a log file
// with its .edc.N archives, and edc never rewrites one to drop a single run:
// rewriting races the writer and can lose output that is being appended.
type logHistoryDeletion struct {
	families []logHistoryDeleteFamily
	targets  map[string]bool
	Runs     int
	Files    int
	Bytes    uint64
	// Shared counts selected runs kept because their file also holds a run that
	// is not selected; Running counts selected runs whose log is still locked.
	Shared, Running int
}

type logHistoryDeleteFamily struct {
	path  string
	runs  int
	bytes uint64
	rows  []logHistoryAttempt
}

func logHistoryRunID(row logHistoryAttempt) string {
	return row.Path + "\x00" + row.Started.Format(time.RFC3339Nano) + "\x00" + strconv.Itoa(row.Ordinal)
}

// planLogHistoryDeletion reads each family of the selected runs again, so a run
// that the caller did not see, such as one written after its last refresh,
// keeps the family.
func planLogHistoryDeletion(targets []logHistoryAttempt) logHistoryDeletion {
	plan := logHistoryDeletion{targets: map[string]bool{}}
	selected := map[string]int{}
	for _, row := range targets {
		plan.targets[logHistoryRunID(row)] = true
		selected[row.Path]++
	}
	paths := make([]string, 0, len(selected))
	for path := range selected {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	families := historyFamiliesIn(paths)
	for _, path := range paths {
		parts, rows, ok := plan.readFamily(families, path)
		switch {
		case !ok:
			plan.Shared += selected[path]
		case logFileLocked(path + ".lock"):
			plan.Running += selected[path]
		case !plan.covers(rows):
			plan.Shared += selected[path]
		default:
			family := logHistoryDeleteFamily{path: path, runs: len(rows), rows: rows}
			for _, part := range parts {
				family.bytes += uint64(part.info.Size())
			}
			plan.families = append(plan.families, family)
			plan.Runs += len(rows)
			plan.Files += len(parts)
			plan.Bytes += family.bytes
		}
	}
	return plan
}

func (plan *logHistoryDeletion) readFamily(families historyFamilies, path string) ([]historyPart, []logHistoryAttempt, bool) {
	parts, err := families.parts(path)
	if err != nil {
		return nil, nil, false
	}
	rows, _, err := readHistoryFamily(path, parts)
	return parts, rows, err == nil && len(rows) > 0
}

func (plan *logHistoryDeletion) covers(rows []logHistoryAttempt) bool {
	for _, row := range rows {
		if row.Outcome == historyOutcomeRunning || !plan.targets[logHistoryRunID(row)] {
			return false
		}
	}
	return true
}

// execute checks every family again just before it removes it. A family that
// gained a run or a lock since the plan is kept and counted as skipped.
func (plan logHistoryDeletion) execute() (deleted logHistoryDeletion, err error) {
	root := defaultLogDirectory()
	paths := make([]string, 0, len(plan.families))
	for _, family := range plan.families {
		paths = append(paths, family.path)
	}
	families := historyFamiliesIn(paths)
	for _, family := range plan.families {
		parts, rows, ok := plan.readFamily(families, family.path)
		if !ok || len(rows) != family.runs || !plan.covers(rows) {
			deleted.Shared += family.runs
			continue
		}
		if logFileLocked(family.path + ".lock") {
			deleted.Running += family.runs
			continue
		}
		var familyErr error
		for _, part := range parts {
			if removeErr := os.Remove(part.path); removeErr != nil && !os.IsNotExist(removeErr) {
				familyErr = errors.Join(familyErr, removeErr)
				continue
			}
			deleted.Files++
			deleted.Bytes += uint64(part.info.Size())
		}
		if familyErr != nil {
			err = errors.Join(err, familyErr)
			continue
		}
		deleted.Runs += family.runs
		if removeErr := os.Remove(family.path + ".lock"); removeErr != nil && !os.IsNotExist(removeErr) {
			err = errors.Join(err, removeErr)
		}
		// Only a command folder that edc made under the default directory is
		// removed, and only when empty; a folder the user named stays.
		if directory := filepath.Dir(family.path); root != "" && filepath.Dir(directory) == root && !strings.HasPrefix(filepath.Base(directory), ".") {
			_ = os.Remove(directory)
		}
	}
	return deleted, err
}
