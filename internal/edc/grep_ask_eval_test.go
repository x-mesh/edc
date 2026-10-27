package edc

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestGrepAskEvaluation(t *testing.T) {
	if os.Getenv("EDC_EVAL_GREP") != "1" {
		t.Skip("set EDC_EVAL_GREP=1 to run the repository retrieval evaluation")
	}
	data, err := os.ReadFile("testdata/grep-ask/queries.json")
	if err != nil {
		t.Fatal(err)
	}
	var queries []struct {
		Query string `json:"query"`
		Path  string `json:"path"`
	}
	if err := json.Unmarshal(data, &queries); err != nil {
		t.Fatal(err)
	}
	targets, _, problems := grepCodeTargets([]string{"."}, grepOptions{})
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	var chunks []grepAskChunk
	type evalChunk struct {
		Path     string `json:"path"`
		Start    int    `json:"start"`
		End      int    `json:"end"`
		Text     string `json:"text"`
		RankPath string `json:"rank_path"`
	}
	var exported []evalChunk
	for _, path := range targets {
		if path == "grep_ask_eval_test.go" || strings.HasPrefix(path, "testdata/grep-ask/") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > grepAskMaxFileBytes || bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
			continue
		}
		for _, chunk := range grepAskChunks(path, data) {
			chunk.result.Path = filepath.ToSlash(filepath.Join("internal/edc", chunk.result.Path))
			if os.Getenv("EDC_EVAL_DUMP") != "" {
				exported = append(exported, evalChunk{Path: chunk.result.Path, Start: chunk.result.Start, End: chunk.result.End, Text: strings.Join(chunk.lines, "\n"), RankPath: chunk.rankPath})
			}
			chunks = append(chunks, chunk)
		}
	}
	if dump := os.Getenv("EDC_EVAL_DUMP"); dump != "" {
		content, err := json.Marshal(exported)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dump, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("evaluated corpus: %d files, %d chunks", len(targets), len(chunks))
	type evalScore struct {
		Path  string  `json:"path"`
		Start int     `json:"start"`
		End   int     `json:"end"`
		Score float64 `json:"score"`
	}
	type queryScores struct {
		Query  string      `json:"query"`
		Scores []evalScore `json:"scores"`
	}
	var scores []queryScores
	hits := 0
	literalHits := 0
	for _, q := range queries {
		matches := 0
		for _, chunk := range chunks {
			if strings.Contains(strings.ToLower(strings.Join(chunk.lines, "\n")), strings.ToLower(q.Query)) {
				matches++
				if chunk.result.Path == q.Path {
					literalHits++
					break
				}
				if matches == 5 {
					break
				}
			}
		}
		if os.Getenv("EDC_EVAL_SCORES") != "" {
			entry := queryScores{Query: q.Query}
			for _, result := range grepAskScore(q.Query, chunks) {
				entry.Scores = append(entry.Scores, evalScore{Path: result.Path, Start: result.Start, End: result.End, Score: result.Score})
			}
			scores = append(scores, entry)
		}
		results := grepAskRank(q.Query, chunks, 5)
		found := false
		for _, result := range results {
			if result.Path == q.Path {
				found = true
				break
			}
		}
		if found {
			hits++
		} else {
			t.Logf("MISS %q expected %s", q.Query, q.Path)
		}
	}
	if output := os.Getenv("EDC_EVAL_SCORES"); output != "" {
		content, err := json.Marshal(scores)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(output, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("literal query top-5: %d/%d (%.1f%%)", literalHits, len(queries), 100*float64(literalHits)/float64(len(queries)))
	t.Logf("keyword top-5: %d/%d (%.1f%%)", hits, len(queries), 100*float64(hits)/float64(len(queries)))
}
