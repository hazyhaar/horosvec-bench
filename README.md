# horosvec-bench

Banc comparatif CGO : horosvec vs sqlite-vec vs hnswlib (un binaire par moteur).

```bash
make libhnsw && make build
bin/gen-random -n 2000 -dim 64 -seed 42 -out /tmp/vectors.jsonl
LD_LIBRARY_PATH=third_party/hnswgo_build bin/bench-hnsw -base /tmp/vectors.jsonl -queries /tmp/vectors.jsonl -holdout 50 -k 10 -sweep "64,128"
```