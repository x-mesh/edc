package edc

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestGrepSemanticRetrievalEvaluation(t *testing.T) {
	const authenticFixtureSchema = 5
	metadata, err := os.ReadFile("testdata/grep-semantic/authentic-retrieval.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		SchemaVersion   int    `json:"schema_version"`
		Model           string `json:"model"`
		ModelRevision   string `json:"model_revision"`
		EncoderRevision string `json:"encoder_revision"`
		VectorFile      string `json:"vector_file"`
		VectorEncoding  string `json:"vector_encoding"`
		Dimensions      int    `json:"dimensions"`
		Corpus          string `json:"corpus"`
		Provenance      struct {
			SourceRevision    string `json:"source_revision"`
			SourceDescription string `json:"source_description"`
			EncoderCommand    string `json:"encoder_command"`
			Selection         string `json:"selection"`
			GenerationCommand string `json:"generation_command"`
		} `json:"provenance"`
		Evaluation struct {
			QueryCount           int     `json:"query_count"`
			ProductionQueryCount int     `json:"production_query_count"`
			FixtureQueryCount    int     `json:"fixture_query_count"`
			ProductionRecallAt5  float64 `json:"production_recall_at_5"`
			ProductionMRRAt10    float64 `json:"production_mrr_at_10"`
			FixtureRecallAt5     float64 `json:"fixture_recall_at_5"`
			FixtureMRRAt10       float64 `json:"fixture_mrr_at_10"`
			Tolerance            float64 `json:"tolerance"`
		} `json:"evaluation"`
		Documents []struct {
			Path        string `json:"path"`
			RankPath    string `json:"rank_path"`
			Start       int    `json:"start"`
			End         int    `json:"end"`
			Text        string `json:"text"`
			VectorIndex int    `json:"vector_index"`
		} `json:"chunks"`
		Queries []struct {
			Kind        string `json:"kind"`
			Query       string `json:"query"`
			Relevant    string `json:"relevant"`
			VectorIndex int    `json:"vector_index"`
		} `json:"queries"`
	}
	if err := json.Unmarshal(metadata, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.SchemaVersion != authenticFixtureSchema || fixture.Model != grepSemanticModel || fixture.ModelRevision != grepMiniLMRevision || !strings.Contains(fixture.EncoderRevision, ":onnx-") || fixture.VectorFile != "authentic-retrieval.f16" || fixture.VectorEncoding == "" || fixture.Dimensions != grepSemanticDimensions || fixture.Corpus == "" || fixture.Provenance.SourceRevision == "" || fixture.Provenance.SourceDescription == "" || fixture.Provenance.EncoderCommand == "" || fixture.Provenance.Selection == "" || fixture.Provenance.GenerationCommand == "" || fixture.Evaluation.QueryCount != len(fixture.Queries) || fixture.Evaluation.ProductionQueryCount <= 0 || fixture.Evaluation.FixtureQueryCount != len(fixture.Queries) || fixture.Evaluation.ProductionQueryCount >= fixture.Evaluation.FixtureQueryCount || fixture.Evaluation.ProductionRecallAt5 < 0.80 || fixture.Evaluation.ProductionMRRAt10 < 0.70 || fixture.Evaluation.FixtureRecallAt5 < 0.80 || fixture.Evaluation.FixtureMRRAt10 < 0.70 || fixture.Evaluation.Tolerance != 0.05 || len(fixture.Documents) == 0 || len(fixture.Queries) == 0 {
		t.Fatalf("invalid fixture: %+v", fixture)
	}
	vectorData, err := os.ReadFile(filepath.Join(filepath.Dir("testdata/grep-semantic/authentic-retrieval.json"), fixture.VectorFile))
	if err != nil {
		t.Fatal(err)
	}
	rowBytes := fixture.Dimensions * 2
	if rowBytes == 0 || len(vectorData)%rowBytes != 0 {
		t.Fatalf("invalid vector file size: %d bytes for %d dimensions", len(vectorData), fixture.Dimensions)
	}
	vectorRows := len(vectorData) / rowBytes
	readVector := func(index int) []float32 {
		if index < 0 || index >= vectorRows {
			t.Fatalf("vector index %d out of range %d", index, vectorRows)
		}
		vector := make([]float32, fixture.Dimensions)
		for i := range vector {
			vector[i] = grepFloat16Table()[binary.LittleEndian.Uint16(vectorData[index*rowBytes+i*2:])]
		}
		return vector
	}
	chunks := make([]grepAskChunk, 0, len(fixture.Documents))
	index := &grepSemanticIndex{Version: grepSemanticVersion, Root: ".", Model: grepSemanticModel, Revision: fixture.ModelRevision, Vectors: make(map[string][]float32)}
	for _, document := range fixture.Documents {
		if document.RankPath == "" || vectorNorm(readVector(document.VectorIndex)) < 0.98 || vectorNorm(readVector(document.VectorIndex)) > 1.02 {
			t.Fatalf("document vector is not normalized: %s", document.Path)
		}
		chunk := grepAskChunk{rankPath: document.RankPath, result: grepAskResult{Path: document.Path, Start: document.Start, End: document.End}, terms: map[string]int{}, size: 1, lines: []string{document.Text}}
		for _, term := range grepAskTokens(document.Text) {
			chunk.terms[term]++
		}
		chunks = append(chunks, chunk)
		index.Vectors[grepSemanticKey(".", chunk)] = readVector(document.VectorIndex)
	}
	topFive, topTen := 0, 0
	totalReciprocal := 0.0
	keywordTopFive := 0
	categoryTopFive := map[string]int{}
	categoryCount := map[string]int{}
	for _, query := range fixture.Queries {
		if query.Kind != "natural_language" && query.Kind != "identifier" && query.Kind != "path" {
			t.Fatalf("unknown query kind %q", query.Kind)
		}
		categoryCount[query.Kind]++
		queryVector := readVector(query.VectorIndex)
		if vectorNorm(queryVector) < 0.98 || vectorNorm(queryVector) > 1.02 {
			t.Fatalf("query vector is not normalized: %s", query.Query)
		}
		semantic := grepSemanticScore(".", grepOptions{pattern: query.Query, askLimit: 10}, chunks, index, queryVector)
		for _, result := range grepAskRank(query.Query, chunks, 5) {
			if result.Path == query.Relevant {
				keywordTopFive++
				break
			}
		}
		position := 0
		for i, result := range semantic {
			if result.Path == query.Relevant {
				position = i + 1
				break
			}
		}
		if position > 0 && position <= 5 {
			topFive++
			categoryTopFive[query.Kind]++
		}
		if position > 0 && position <= 10 {
			topTen++
			totalReciprocal += 1 / float64(position)
		}
	}
	if categoryCount["identifier"] == 0 || categoryCount["path"] == 0 {
		t.Fatalf("fixture must contain identifier and path queries: counts=%v", categoryCount)
	}
	count := len(fixture.Queries)
	if fixture.Evaluation.QueryCount != count {
		t.Fatalf("evaluation query count=%d, fixture queries=%d", fixture.Evaluation.QueryCount, count)
	}
	recall := float64(topFive) / float64(count)
	mrr := totalReciprocal / float64(count)
	keywordRecall := float64(keywordTopFive) / float64(count)
	identifierPathCount := categoryCount["identifier"] + categoryCount["path"]
	identifierPathRecall := float64(categoryTopFive["identifier"]+categoryTopFive["path"]) / float64(identifierPathCount)
	t.Logf("semantic Recall@5=%.9f MRR@10=%.9f keyword Recall@5=%.1f%% delta=%.1fpp identifier/path Recall@5=%.1f%% (n=%d)", recall*100, mrr, keywordRecall*100, (recall-keywordRecall)*100, identifierPathRecall*100, identifierPathCount)
	if math.Abs(recall-fixture.Evaluation.FixtureRecallAt5) > fixture.Evaluation.Tolerance || math.Abs(mrr-fixture.Evaluation.FixtureMRRAt10) > fixture.Evaluation.Tolerance || math.Abs(recall-fixture.Evaluation.ProductionRecallAt5) > fixture.Evaluation.Tolerance || math.Abs(mrr-fixture.Evaluation.ProductionMRRAt10) > fixture.Evaluation.Tolerance || recall < 0.80 || mrr < 0.70 || recall-keywordRecall < 0.20 || identifierPathRecall != 1 || categoryTopFive["identifier"] != categoryCount["identifier"] || categoryTopFive["path"] != categoryCount["path"] {
		t.Fatalf("retrieval thresholds failed: recall=%.3f mrr=%.3f keyword=%.3f identifier/path=%.3f categories=%v/%v", recall, mrr, keywordRecall, identifierPathRecall, categoryTopFive, categoryCount)
	}
}

func vectorNorm(vector []float32) float64 {
	sum := 0.0
	for _, value := range vector {
		sum += float64(value) * float64(value)
	}
	return math.Sqrt(sum)
}

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
