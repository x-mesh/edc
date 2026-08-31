package edc

import "time"

type Status string

const (
	StatusPass Status = "pass"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
	StatusSkip Status = "skip"
)

type DiagnosticError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type Evidence struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type Result struct {
	Probe      string                 `json:"probe"`
	Status     Status                 `json:"status"`
	StartedAt  time.Time              `json:"started_at"`
	DurationMS int64                  `json:"duration_ms"`
	Summary    string                 `json:"summary"`
	Metrics    map[string]interface{} `json:"metrics,omitempty"`
	Evidence   []Evidence             `json:"evidence,omitempty"`
	Warnings   []string               `json:"warnings,omitempty"`
	Error      *DiagnosticError       `json:"error,omitempty"`
}

type ToolInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type RunInfo struct {
	ID         string    `json:"id"`
	StartedAt  time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
}

type RedactionInfo struct {
	Enabled bool `json:"enabled"`
}

type Summary struct {
	Pass int `json:"pass"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
	Skip int `json:"skip"`
}

type Report struct {
	SchemaVersion string                 `json:"schema_version"`
	Tool          ToolInfo               `json:"tool"`
	Run           RunInfo                `json:"run"`
	Target        map[string]interface{} `json:"target,omitempty"`
	Host          map[string]interface{} `json:"host,omitempty"`
	Results       []Result               `json:"results"`
	Summary       Summary                `json:"summary"`
	Redaction     RedactionInfo          `json:"redaction"`
}

func summarize(results []Result) Summary {
	var summary Summary
	for _, result := range results {
		switch result.Status {
		case StatusPass:
			summary.Pass++
		case StatusWarn:
			summary.Warn++
		case StatusFail:
			summary.Fail++
		case StatusSkip:
			summary.Skip++
		}
	}
	return summary
}

func resultFromError(probe string, started time.Time, kind string, err error) Result {
	return Result{
		Probe: probe, Status: StatusFail, StartedAt: started.UTC(),
		DurationMS: time.Since(started).Milliseconds(),
		Summary:    err.Error(),
		Error:      &DiagnosticError{Kind: kind, Message: err.Error()},
	}
}
