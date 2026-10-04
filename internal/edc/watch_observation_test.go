package edc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamWatchEmptyDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	var output bytes.Buffer
	code := streamWatch(ctx, &output, time.Millisecond, time.Second, true, false, func(ctx context.Context) Result {
		<-ctx.Done()
		return Result{Status: StatusFail}
	})
	if code != 2 {
		t.Fatalf("empty deadline exit = %d, want 2; output=%s", code, &output)
	}
}

func watchObservationLines(t *testing.T, output *bytes.Buffer) []map[string]json.RawMessage {
	t.Helper()
	decoder := json.NewDecoder(output)
	var lines []map[string]json.RawMessage
	for {
		var line map[string]json.RawMessage
		if err := decoder.Decode(&line); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	return lines
}

func TestStreamWatchObservationContexts(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		first                         Status
		cancelledBefore, childTimeout bool
		wantCode, wantSamples         int
	}{
		{name: "already cancelled", cancelledBefore: true, wantCode: 4},
		{name: "cancel first probe", wantCode: 4},
		{name: "pass before interruption", first: StatusPass, wantSamples: 1},
		{name: "fail before interruption", first: StatusFail, wantCode: 1, wantSamples: 1},
		{name: "child timeout is failure", childTimeout: true, wantCode: 1, wantSamples: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelledBefore {
				cancel()
			}
			calls := 0
			var output bytes.Buffer
			code := streamWatch(ctx, &output, time.Millisecond, time.Millisecond, true, false, func(sampleCtx context.Context) Result {
				calls++
				if calls == 1 && tc.first != "" {
					return Result{Status: tc.first, DurationMS: 7}
				}
				if calls == 1 && tc.childTimeout {
					<-sampleCtx.Done()
					if ctx.Err() != nil {
						t.Fatal("parent stopped before child timeout")
					}
					return Result{Status: StatusFail, DurationMS: 7}
				}
				cancel()
				<-sampleCtx.Done()
				return Result{Status: StatusFail, DurationMS: 99}
			})
			if code != tc.wantCode {
				t.Fatalf("exit=%d, want %d", code, tc.wantCode)
			}
			lines := watchObservationLines(t, &output)
			if len(lines) != tc.wantSamples+1 {
				t.Fatalf("lines=%d, want %d", len(lines), tc.wantSamples+1)
			}
			summary := lines[len(lines)-1]
			wantStatus := "no_samples"
			if tc.wantSamples > 0 {
				wantStatus = "observed"
			}
			if string(summary["type"]) != `"summary"` || string(summary["observation_status"]) != `"`+wantStatus+`"` || string(summary["stop_reason"]) != `"cancelled"` {
				t.Fatalf("summary=%v", summary)
			}
			for _, field := range []string{"samples", "pass", "warn", "fail", "duration_ms", "min_ms", "avg_ms", "p95_ms", "max_ms", "longest_failure_ms"} {
				if _, ok := summary[field]; !ok {
					t.Fatalf("missing %s", field)
				}
				if tc.wantSamples == 0 && field != "duration_ms" && string(summary[field]) != "0" {
					t.Fatalf("empty %s=%s", field, summary[field])
				}
			}
			if tc.wantSamples > 0 {
				if string(summary["samples"]) != "1" {
					t.Fatalf("samples=%s", summary["samples"])
				}
				for _, field := range []string{"min_ms", "avg_ms", "p95_ms", "max_ms"} {
					if string(summary[field]) != "7" {
						t.Fatalf("%s=%s", field, summary[field])
					}
				}
				count := "pass"
				if tc.first == StatusFail || tc.childTimeout {
					count = "fail"
				}
				if string(summary[count]) != "1" {
					t.Fatalf("%s=%s", count, summary[count])
				}
			}
		})
	}
}

func TestStreamWatchEmptyHTTPDuration(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var output bytes.Buffer
	code := streamWatch(ctx, &output, time.Millisecond, time.Second, true, false, func(ctx context.Context) Result {
		return probeHTTPWithOptions(ctx, server.URL, httpCheckOptions{})
	})
	select {
	case <-entered:
	default:
		t.Fatal("local HTTP probe did not reach server")
	}
	if code != 2 {
		t.Fatalf("exit=%d", code)
	}
	lines := watchObservationLines(t, &output)
	if len(lines) != 1 {
		t.Fatalf("lines=%d", len(lines))
	}
	summary := lines[0]
	if string(summary["observation_status"]) != `"no_samples"` || string(summary["stop_reason"]) != `"duration"` || string(summary["fail"]) != "0" || string(summary["samples"]) != "0" {
		t.Fatalf("summary=%v", summary)
	}
}

func TestStreamWatchEmptyText(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range []string{"en", "ko", "ja"} {
		setLanguage(language)
		for _, duration := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			reason := "cancelled"
			if duration {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				reason = "duration"
			} else {
				cancel()
			}
			var output bytes.Buffer
			streamWatch(ctx, &output, time.Millisecond, time.Second, false, false, func(context.Context) Result { t.Fatal("unexpected probe"); return Result{} })
			cancel()
			if !strings.Contains(output.String(), T("observe.watch.no_samples_"+reason)) {
				t.Fatalf("missing reason: %s", &output)
			}
			if strings.Contains(output.String(), T("observe.watch.latency_summary", int64(0), int64(0), int64(0), int64(0), time.Duration(0))) {
				t.Fatalf("unmeasured latency: %s", &output)
			}
		}
	}
}

type watchRejectSummary struct{}

func (watchRejectSummary) Write([]byte) (int, error) { return 0, errors.New("summary write rejected") }

func TestStreamWatchSummaryWriteFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := streamWatch(ctx, watchRejectSummary{}, time.Millisecond, time.Second, true, false, func(context.Context) Result { return Result{} }); code != 2 {
		t.Fatalf("exit=%d, want 2", code)
	}
}

func TestStreamWatchObservedTextKeepsLatency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output bytes.Buffer
	calls := 0
	code := streamWatch(ctx, &output, time.Millisecond, time.Second, false, false, func(context.Context) Result {
		calls++
		if calls > 1 {
			cancel()
		}
		return Result{Status: StatusPass, DurationMS: 7}
	})
	if code != 0 || !strings.Contains(output.String(), T("observe.watch.latency_summary", int64(7), int64(7), int64(7), int64(7), time.Duration(0))) {
		t.Fatalf("exit=%d output=%s", code, &output)
	}
}
