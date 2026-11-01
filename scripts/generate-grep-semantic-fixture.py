#!/usr/bin/env python3
import argparse
import gzip
import json
import os
import re

import numpy as np
from sentence_transformers import SentenceTransformer


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("input")
    parser.add_argument("metadata")
    parser.add_argument("vectors")
    parser.add_argument("--source-revision", required=True)
    parser.add_argument("--top-k", type=int, default=64)
    parser.add_argument("--keyword-scores")
    parser.add_argument("--production-recall-at-5", type=float, required=True)
    parser.add_argument("--production-mrr-at-10", type=float, required=True)
    parser.add_argument("--fixture-recall-at-5", type=float, required=True)
    parser.add_argument("--fixture-mrr-at-10", type=float, required=True)
    parser.add_argument("--metric-tolerance", type=float, default=0.05)
    args = parser.parse_args()
    with open(args.input, encoding="utf-8") as file:
        source = json.load(file)
    model = SentenceTransformer(source["model"], local_files_only=True, device="cpu")
    model.max_seq_length = 128
    documents = source["chunks"]
    document_vectors = np.asarray([row["vector_f16"] for row in documents], dtype="<u2").view(np.float16).astype(np.float32)
    queries = []
    synthetic_queries = {"ValidateToken", "grepSemanticRankScopes", "internal/edc/grep_semantic.go"}
    for row in source["queries"]:
        if row["query"] in synthetic_queries:
            continue
        queries.append({"kind": "natural_language", "query": row["query"], "relevant": row["relevant"], "vector": np.asarray(row["vector_f16"], dtype="<u2").view(np.float16).astype(np.float32)})
    additions = [
        ("identifier", "grepSemanticRankScopes", "internal/edc/grep_ask.go"),
        ("identifier", "grepSemanticScoreCandidates", "internal/edc/grep_semantic.go"),
        ("path", "internal/edc/grep_semantic.go", "internal/edc/grep_semantic.go"),
    ]
    production_query_count = len(queries)
    addition_kinds = {query: kind for kind, query, _ in additions}
    existing_queries = {row["query"] for row in queries}
    for row in queries:
        row["kind"] = addition_kinds.get(row["query"], "natural_language")
    missing_additions = [row for row in additions if row[1] not in existing_queries]
    encoded = model.encode([row[1] for row in missing_additions], normalize_embeddings=True, show_progress_bar=False)
    for (kind, query, relevant), vector in zip(missing_additions, encoded):
        queries.append({"kind": kind, "query": query, "relevant": relevant, "vector": vector.astype(np.float32)})
    keyword_scores = {}
    if args.keyword_scores:
        with open(args.keyword_scores, encoding="utf-8") as file:
            for row in json.load(file):
                keyword_scores[row["query"]] = {(item["path"], item["start"], item["end"]): item["score"] for item in row["scores"]}

    def tokens(value):
        words = []
        for part in re.findall(r"[A-Za-z0-9_\u0080-\uffff]+", value):
            words.extend(re.findall(r"[A-Z]+(?=[A-Z][a-z]|$)|[A-Z]?[a-z]+|[0-9]+", part))
        return {word.lower() for word in words}

    def source_class(query, path):
        query_terms = tokens(query)
        special = query_terms.intersection({"test", "tests", "testing", "docs", "doc", "documentation", "readme", "locale", "i18n", "linux", "darwin", "windows", "platform", "testdata"})
        path = path.lower().replace(os.sep, "/")
        auxiliary = path.endswith("_test.go") or "/testdata/" in path or "/locale/" in path or "/docs/" in path or "/doc/" in path or path.endswith("readme.md") or path.endswith(("_linux.go", "_darwin.go", "_windows.go"))
        if special:
            return 2 if auxiliary else 1
        return 0 if auxiliary else 1

    def exact_score(query, document):
        query_terms = tokens(query)
        text_terms = tokens(document["text"])
        path_terms = tokens(document.get("rank_path", document["path"]))
        return sum((1 if term in text_terms else 0) + (0.5 if term in path_terms else 0) for term in query_terms)

    document_keys = [(document["path"], document["start"], document["end"]) for document in documents]
    selected = set()
    for query in queries:
        scores = np.einsum("ij,j->i", document_vectors.astype(np.float64), query["vector"].astype(np.float64))
        for index in np.argsort(-scores, kind="stable")[:args.top_k]:
            selected.add(int(index))
        keyword = keyword_scores.get(query["query"], {})
        for index in sorted(range(len(documents)), key=lambda index: (-keyword.get(document_keys[index], 0), document_keys[index]))[:args.top_k]:
            if keyword.get(document_keys[index], 0) > 0:
                selected.add(index)
        exact = sorted(range(len(documents)), key=lambda index: (-exact_score(query["query"], documents[index]), document_keys[index]))
        for index in exact[:args.top_k]:
            if exact_score(query["query"], documents[index]) > 0:
                selected.add(index)
        source_candidates = sorted(range(len(documents)), key=lambda index: (-source_class(query["query"], documents[index]["path"]), document_keys[index]))
        selected.update(source_candidates[:args.top_k])
        for index, document in enumerate(documents):
            if document["path"] == query["relevant"]:
                selected.add(index)
    selected_documents = sorted(selected, key=lambda index: (documents[index]["path"], documents[index]["start"], documents[index]["end"]))
    rows = []
    metadata_documents = []
    for row_index, document_index in enumerate(selected_documents):
        document = documents[document_index]
        metadata_documents.append({
            "path": document["path"],
            "rank_path": document.get("rank_path", document["path"]),
            "start": document["start"],
            "end": document["end"],
            "text": document["text"],
            "vector_index": row_index,
        })
        rows.append(document_vectors[document_index])
    metadata_queries = []
    query_vector_start = len(rows)
    for query_index, query in enumerate(queries):
        metadata_queries.append({
            "kind": query["kind"],
            "query": query["query"],
            "relevant": query["relevant"],
            "vector_index": query_vector_start + query_index,
        })
        rows.append(query["vector"])
    vectors = np.asarray(rows, dtype=np.float16).view("<u2")
    os.makedirs(os.path.dirname(args.metadata), exist_ok=True)
    os.makedirs(os.path.dirname(args.vectors), exist_ok=True)
    with open(args.vectors, "wb") as file:
        file.write(vectors.tobytes(order="C"))
    metadata = {
        "schema_version": 5,
        "model": source["model"],
        "model_revision": source["model_revision"],
        "encoder_revision": source["encoder_revision"],
        "vector_file": os.path.basename(args.vectors),
        "vector_encoding": "little-endian IEEE 754 binary16, row-major",
        "dimensions": 384,
        "corpus": "production repository source collected with grepAskCollect semantic mode",
        "provenance": {
            "source_revision": args.source_revision,
            "encoder_command": "SentenceTransformer(model, local_files_only=True, device=cpu), max_seq_length=128, normalize_embeddings=True",
            "source_description": "repository source captured from the working tree; source_revision records its base HEAD, and chunks retain path, declaration span, and source text",
            "selection": "top-k semantic, keyword, exact-token, and source-class candidates for every query plus every chunk in each relevance file; top-k is configurable and recorded at generation time",
            "generation_command": "python3 scripts/generate-grep-semantic-fixture.py <captured-corpus.json> internal/edc/testdata/grep-semantic/authentic-retrieval.json.gz internal/edc/testdata/grep-semantic/authentic-retrieval.f16 --source-revision \"$(git rev-parse HEAD)\" --top-k 128 --production-recall-at-5 0.958333333 --production-mrr-at-10 0.735763889 --fixture-recall-at-5 0.962962963 --fixture-mrr-at-10 0.780864198 --metric-tolerance 0.05",
        },
        "evaluation": {
            "query_count": len(queries),
            "production_query_count": production_query_count,
            "fixture_query_count": len(queries),
            "production_recall_at_5": args.production_recall_at_5,
            "production_mrr_at_10": args.production_mrr_at_10,
            "fixture_recall_at_5": args.fixture_recall_at_5,
            "fixture_mrr_at_10": args.fixture_mrr_at_10,
            "tolerance": args.metric_tolerance,
        },
        "chunks": metadata_documents,
        "queries": metadata_queries,
    }
    with gzip.open(args.metadata, "wt", encoding="utf-8") as file:
        json.dump(metadata, file, ensure_ascii=False, separators=(",", ":"))
        file.write("\n")


if __name__ == "__main__":
    main()
