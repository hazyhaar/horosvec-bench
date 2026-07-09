# Benchmark results — 2026-07-09

Three engines, common protocol (queries never in the base, warm-up excluded,
monotonic-clock windows >= 3s, recall measured sequentially, exact ground truth).
Machine: 32 cores, 64 GB RAM, NVMe. One engine at a time.

Corpora:
- `siftA_*`: SIFT-1M (128-dim, 10k queries, official ground truth).
- `realB_*`: 1M real 512-dim embeddings (qwen3-embedding-0.6B over Hacker News
  text), 500 held-out queries, exact ground truth computed by the harness.
- `cliff300k`: diagnostic run (see below).

Engine files:
- `*_horosvec.jsonl`: **DB-blob mode** (rerank = row-by-row SQL blob reads) —
  the non-production path, kept published because its two apparent defects
  (throughput cliff, dead concurrency scaling) were instructive: perf showed
  41% GC + 21.6% database/sql.withLock. See the repo README of horosvec.
- `*_horosvec_arena.jsonl`: **arena mode** (production: mmap fp16 rerank,
  zero SQL in the hot loop). The cliff disappears; 32-client throughput ×56.
- `*_hnsw.jsonl`: hnswlib via cgo binding (third_party wrapper).
- `*_sqlitevec.jsonl`: sqlite-vec (exact scan, recall control).

Each line: one (sweep parameter × client concurrency) point with recall@10,
aggregate QPS, p50/p99 ms, build seconds. The `concurrency` field is the number
of closed-loop client goroutines.
