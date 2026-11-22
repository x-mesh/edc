package edc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

type collectionState string

const (
	collectionObserved         collectionState = "observed"
	collectionPartial          collectionState = "partial"
	collectionPermissionDenied collectionState = "permission_denied"
	collectionUnsupported      collectionState = "unsupported"
	collectionUnavailable      collectionState = "unavailable"
)

type collectionObservation struct {
	State    collectionState `json:"state"`
	Observed *int            `json:"observed,omitempty"`
	Reason   string          `json:"reason,omitempty"`
}

func withCollection(result Result, state collectionState, observed *int, reason string) Result {
	result.Collection = &collectionObservation{State: state, Observed: observed, Reason: reason}
	return result
}

func collectionReadError(result Result, err error) Result {
	state := collectionUnavailable
	if errors.Is(err, os.ErrPermission) {
		state = collectionPermissionDenied
	}
	return withCollection(result, state, nil, infoSingleLine(err.Error()))
}

func collectionStateText(state collectionState) string {
	switch state {
	case collectionObserved, collectionPartial, collectionPermissionDenied, collectionUnsupported, collectionUnavailable:
		return T("collection.state." + string(state))
	}
	return topPrintableText(string(state))
}

func collectionText(observation *collectionObservation) string {
	if observation == nil {
		return ""
	}
	parts := []string{collectionStateText(observation.State)}
	if observation.Observed != nil {
		parts = append(parts, T("collection.observed_count", *observation.Observed))
	}
	if observation.Reason != "" {
		parts = append(parts, topPrintableText(infoSingleLine(observation.Reason)))
	}
	return strings.Join(parts, " · ")
}

func printCollectionDetail(writer io.Writer, result Result, verbose, color bool) {
	printCollectionDetailWithStyle(writer, result, verbose, newInfoStyle(color))
}

func printCollectionDetailWithStyle(writer io.Writer, result Result, verbose bool, style infoStyle) {
	observation := result.Collection
	if observation == nil || (observation.State == collectionObserved && !verbose) {
		return
	}
	text := T("collection.detail", collectionText(observation))
	if observation.State != collectionObserved {
		text = style.warn(text)
	} else {
		text = style.dim(text)
	}
	style.write(writer, "      ", "      ", text)
}

func collectionSummary(results []Result) []string {
	counts := map[collectionState]int{}
	for _, result := range results {
		if result.Collection != nil {
			counts[result.Collection.State]++
		}
	}
	var parts []string
	for _, state := range []collectionState{collectionObserved, collectionPartial, collectionPermissionDenied, collectionUnsupported, collectionUnavailable} {
		if count := counts[state]; count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count, collectionStateText(state)))
		}
	}
	return parts
}

func printCollectionSummary(writer io.Writer, results []Result, color bool) {
	printCollectionSummaryWithStyle(writer, results, newInfoStyle(color))
}

func printCollectionSummaryWithStyle(writer io.Writer, results []Result, style infoStyle) {
	parts := collectionSummary(results)
	if len(parts) == 0 {
		return
	}
	parts[0] = T("collection.summary", parts[0])
	style.line(writer, style.join(" · ", parts...))
}
