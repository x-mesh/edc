package edc

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

type cleanProgressContextKey struct{}

type cleanProgressTask struct {
	stage, target string
	started       time.Time
	finished      time.Time
	visible       bool
	skipped       bool
}

type cleanProgress struct {
	mutex      sync.Mutex
	writer     io.Writer
	home       string
	redact     bool
	started    time.Time
	now        func() time.Time
	next       uint64
	tasks      map[uint64]cleanProgressTask
	stages     map[string]cleanProgressTask
	lastDetail time.Time
	cancelled  func() bool
}

func newCleanProgress(writer io.Writer, home string, redact bool) *cleanProgress {
	return &cleanProgress{writer: writer, home: home, redact: redact, started: time.Now(), now: time.Now, tasks: map[uint64]cleanProgressTask{}, stages: map[string]cleanProgressTask{}}
}

func (progress *cleanProgress) line(state string, task cleanProgressTask, now time.Time) {
	target := progress.targetText(task.target)
	fmt.Fprintf(progress.writer, "[%6.1fs] %-5s %s", now.Sub(progress.started).Seconds(), state, T("clean.progress.stage."+task.stage))
	if target != "" {
		fmt.Fprintf(progress.writer, "  %s", target)
	}
	if state != "START" {
		fmt.Fprintf(progress.writer, "  (%s)", now.Sub(task.started).Round(10*time.Millisecond))
	}
	fmt.Fprintln(progress.writer)
}

func (progress *cleanProgress) begin(stage, target string) func() {
	progress.mutex.Lock()
	now := progress.now()
	progress.next++
	id := progress.next
	detail := stage == "tool" || stage == "measure"
	visible := !detail || now.Sub(progress.lastDetail) >= 2*time.Second
	task := cleanProgressTask{stage: stage, target: target, started: now, visible: visible}
	if progress.cancelled != nil {
		task.skipped = progress.cancelled()
	}
	progress.tasks[id] = task
	if cleanProgressStage(stage) {
		progress.stages[stage] = task
	}
	if visible {
		progress.line("START", task, now)
		if detail {
			progress.lastDetail = now
		}
	}
	progress.mutex.Unlock()
	return func() {
		progress.mutex.Lock()
		defer progress.mutex.Unlock()
		now := progress.now()
		delete(progress.tasks, id)
		if cleanProgressStage(stage) {
			task.finished = now
			progress.stages[stage] = task
		}
		if task.visible || now.Sub(task.started) >= time.Second {
			progress.line("END", task, now)
		}
	}
}

func (progress *cleanProgress) heartbeat() {
	progress.mutex.Lock()
	defer progress.mutex.Unlock()
	var newest uint64
	for id := range progress.tasks {
		if id > newest {
			newest = id
		}
	}
	if newest != 0 {
		progress.line("RUN", progress.tasks[newest], progress.now())
	}
}

func (progress *cleanProgress) run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			progress.heartbeat()
		}
	}
}

func (deps cleanDeps) begin(stage, target string) func() {
	if deps.progress == nil {
		return func() {}
	}
	return deps.progress(stage, target)
}

func cleanStep[T any](deps cleanDeps, stage, target string, run func() T) T {
	done := deps.begin(stage, target)
	defer done()
	return run()
}

func cleanProgressDeps(deps cleanDeps, progress *cleanProgress) cleanDeps {
	deps.progress = progress.begin
	run, measure := deps.run, deps.measure
	deps.run = func(ctx context.Context, spec cleanToolSpec) cleanToolOutput {
		args := spec.Args
		if spec.Name == "git" && len(args) >= 2 && args[0] == "-c" {
			args = args[2:]
		}
		name := spec.Name
		if len(args) > 0 {
			name += " " + args[0]
		}
		return cleanStep(deps, "tool", name+" · "+spec.Dir, func() cleanToolOutput { return run(ctx, spec) })
	}
	deps.measure = func(ctx context.Context, options duOptions, sources duSources) (duScan, error) {
		done := deps.begin("measure", options.root)
		defer done()
		return measure(ctx, options, sources)
	}
	return deps
}

var cleanProgressStages = []string{"caches", "projects", "docker", "docker_root", "system", "vm", "worktrees", "newest"}

func cleanProgressStage(stage string) bool {
	for _, name := range cleanProgressStages {
		if name == stage {
			return true
		}
	}
	return false
}

func (progress *cleanProgress) targetText(target string) string {
	if progress.home != "" {
		target = strings.ReplaceAll(target, progress.home, "~")
	}
	target = historyTerminalText(target)
	if progress.redact {
		target = redactIPAddresses(target)
	}
	return target
}

type cleanProgressSnapshot struct {
	at, started time.Time
	stages      map[string]cleanProgressTask
	active      []cleanProgressTask
}

func (progress *cleanProgress) snapshot() cleanProgressSnapshot {
	progress.mutex.Lock()
	defer progress.mutex.Unlock()
	snapshot := cleanProgressSnapshot{at: progress.now(), started: progress.started, stages: make(map[string]cleanProgressTask, len(progress.stages))}
	for stage, task := range progress.stages {
		snapshot.stages[stage] = task
	}
	ids := make([]uint64, 0, len(progress.tasks))
	for id := range progress.tasks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		task := progress.tasks[id]
		if task.stage == "total" || cleanProgressStage(task.stage) {
			continue
		}
		task.target = progress.targetText(task.target)
		snapshot.active = append(snapshot.active, task)
	}
	return snapshot
}
