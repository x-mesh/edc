#!/usr/bin/env python3
import argparse
import json
import os
import time

os.environ["HF_HUB_OFFLINE"] = "1"
os.environ["TRANSFORMERS_OFFLINE"] = "1"
os.environ["HF_HUB_DISABLE_TELEMETRY"] = "1"

import numpy as np
import torch
from sentence_transformers import SentenceTransformer


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("corpus")
    parser.add_argument("queries")
    parser.add_argument("--keyword-scores")
    parser.add_argument("--model", default="sentence-transformers/all-MiniLM-L6-v2")
    parser.add_argument("--e5-prefix", action="store_true")
    parser.add_argument("--threads", type=int, default=4)
    args = parser.parse_args()
    with open(args.corpus, encoding="utf-8") as f:
        corpus = json.load(f)
    with open(args.queries, encoding="utf-8") as f:
        queries = json.load(f)
    keyword_scores = None
    if args.keyword_scores:
        with open(args.keyword_scores, encoding="utf-8") as f:
            keyword_scores = json.load(f)
        if [item["query"] for item in keyword_scores] != [item["query"] for item in queries]:
            raise ValueError("keyword scores do not match the query set")
    torch.set_num_threads(args.threads)
    started = time.monotonic()
    model = SentenceTransformer(args.model, local_files_only=True, device="cpu")
    model.max_seq_length = 128
    passages = [("passage: " if args.e5_prefix else "") + item.get("rank_path", item["path"]) + "\n" + item["text"][:3000] for item in corpus]
    vectors = model.encode(passages, batch_size=32, normalize_embeddings=True, show_progress_bar=False)
    if not np.isfinite(vectors).all():
        raise ValueError("passage embeddings contain non-finite values")
    indexed = time.monotonic()
    questions = model.encode([("query: " if args.e5_prefix else "") + item["query"] for item in queries], batch_size=24, normalize_embeddings=True, show_progress_bar=False)
    if not np.isfinite(questions).all():
        raise ValueError("query embeddings contain non-finite values")
    reports = {"semantic": {"hits": 0, "misses": []}}
    if keyword_scores is not None:
        for name in ("semantic+keyword", "hybrid-90-10", "hybrid-75-25"):
            reports[name] = {"hits": 0, "misses": []}
    chunk_ids = [(c["path"], c["start"], c["end"]) for c in corpus]
    for query_index, (question, vector) in enumerate(zip(queries, questions)):
        with np.errstate(over="ignore", invalid="ignore", divide="ignore"):
            semantic_scores = vectors @ vector
        if not np.isfinite(semantic_scores).all():
            raise ValueError("similarity scores contain non-finite values")
        rankings = {"semantic": semantic_scores}
        if keyword_scores is not None:
            entries = keyword_scores[query_index]["scores"] or []
            by_chunk = {(x["path"], x["start"], x["end"]): x["score"] for x in entries}
            maximum = max(by_chunk.values(), default=0)
            lexical = np.array([by_chunk.get(key, 0) / maximum if maximum else 0 for key in chunk_ids])
            rankings["semantic+keyword"] = 0.95 * semantic_scores + 0.05 * lexical
            rankings["hybrid-90-10"] = 0.90 * semantic_scores + 0.10 * lexical
            rankings["hybrid-75-25"] = 0.75 * semantic_scores + 0.25 * lexical
        for method, scores in rankings.items():
            order = np.argsort(-scores, kind="stable")
            selected = []
            per_file = {}
            for index in order:
                path = corpus[int(index)]["path"]
                if per_file.get(path, 0) >= 2:
                    continue
                per_file[path] = per_file.get(path, 0) + 1
                selected.append(path)
                if len(selected) == 5:
                    break
            if question["path"] in selected:
                reports[method]["hits"] += 1
            else:
                reports[method]["misses"].append({"query": question["query"], "expected": question["path"], "top5": selected})
    print(json.dumps({"model": args.model, "chunks": len(corpus), "questions": len(queries), "index_seconds": round(indexed - started, 2), "total_seconds": round(time.monotonic() - started, 2), "reports": reports}, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
