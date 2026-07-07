.PHONY: libhnsw build clean

HNSW_SRC := $(shell ls -d $(HOME)/go/pkg/mod/github.com/evan176/hnswgo@* 2>/dev/null | head -1)
HNSW_BUILD := $(CURDIR)/third_party/hnswgo_build
CGO_LDFLAGS := -L$(HNSW_BUILD)
export CGO_LDFLAGS

libhnsw:
	@test -n "$(HNSW_SRC)" || (echo "hnswgo module introuvable dans GOPATH/pkg/mod" && exit 1)
	rm -rf $(HNSW_BUILD)
	mkdir -p $(HNSW_BUILD)
	cp -r $(HNSW_SRC)/* $(HNSW_BUILD)/
	chmod -R u+w $(HNSW_BUILD)
	$(MAKE) -C $(HNSW_BUILD)

build: libhnsw
	GOWORK=off CGO_ENABLED=1 go build -o bin/bench-horosvec ./cmd/bench-horosvec
	GOWORK=off CGO_ENABLED=1 CGO_LDFLAGS="$(CGO_LDFLAGS)" go build -o bin/bench-sqlitevec ./cmd/bench-sqlitevec
	GOWORK=off CGO_ENABLED=1 CGO_LDFLAGS="$(CGO_LDFLAGS)" go build -o bin/bench-hnsw ./cmd/bench-hnsw
	GOWORK=off go build -o bin/gen-random ./cmd/gen-random

clean:
	rm -rf bin $(HNSW_BUILD)