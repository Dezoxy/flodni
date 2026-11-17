# Operator entry point: the Go gates and the architecture workspace.
#
# The architecture targets come from development-base and run locally with
# Docker. The Go targets need Linux with libfuse3-dev (the FUSE layer is cgo). Override any
# variable per call, e.g. `make view PORT=8081`.

# ── pins ─────────────────────────────────────────────────────────────────────
# The Structurizr image your viewer runs. Keep it identical to the server's pin
# so the parser here is the parser there. The PNG/SVG export uses its
# -playwright tag.
STRUCTURIZR_IMAGE ?= structurizr/structurizr:2026.10.26
# Pandoc with LaTeX and the Eisvogel template, for `make pdf` (~2 GB).
PANDOC_IMAGE      ?= pandoc/extra:3.11.0.0-debian
# Mermaid CLI, for `make mermaid-render` and `make pdf`: draws Mermaid blocks
# as PNG. The mature 11.x line; runs as your user (~630 MB).
MERMAID_IMAGE     ?= minlag/mermaid-cli:12.0.1
# promtool, for the metrics check in `make e2e`: `promtool check metrics` reads the
# scrape. Pinned by digest; the tag is for the reader.
# renovate: datasource=docker depName=prom/prometheus
PROMTOOL_IMAGE    ?= prom/prometheus:v3.16.0@sha256:e04987641b57704f408e31f6373ba4138ecf27cb4a165d24aabb6c38fddd0f06

# ── paths ────────────────────────────────────────────────────────────────────
ARCH_DIR  ?= docs/architecture
GENERATED := $(ARCH_DIR)/generated
PORT      ?= 8080

STRUCTURIZR := docker run --rm -v "$(CURDIR)/$(ARCH_DIR):/w:ro"

.PHONY: help go-check errcheck go-test e2e validate inspect check docs test view export mermaid-views mermaid-render mermaid pdf clean
.DEFAULT_GOAL := help

## help            list the targets
help:
	@awk 'BEGIN { FS = "## " } /^## / { print "  " $$2 }' $(MAKEFILE_LIST)

# The vendored engine keeps its own formatting and is not vetted or tested here.
GO_PKGS = $$(go list ./... | grep -v 'internal/anacrolix-torrent')

# Pinned: a new errcheck release can add findings. Renovate proposes it in its
# own pull request, where CI shows them.
ERRCHECK = github.com/kisielk/errcheck@v1.30.0

## errcheck        fail on an unchecked error outside .errcheck-excludes (Linux, libfuse3-dev)
errcheck:
	go run $(ERRCHECK) -ignoretests -exclude .errcheck-excludes $(GO_PKGS)

## go-check        gofmt, build, vet and errcheck the Go code (Linux, libfuse3-dev)
go-check:
	@out=$$(gofmt -l . | grep -v '^internal/anacrolix-torrent/' || true); \
	if [ -n "$$out" ]; then echo "not gofmt clean:"; echo "$$out"; exit 1; fi
	go build ./...
	go vet $(GO_PKGS)
	$(MAKE) errcheck

## go-test         Go tests with the race detector (Linux, libfuse3-dev)
go-test:
	go test -race -count=1 $(GO_PKGS)
	cd internal/anacrolix-torrent && go test -race -count=1 -run 'Private|PeerRequestServer|PeerWake|ProcessCounters|Malformed|RefillV2|RefillFlagOff' .
	cd internal/anacrolix-torrent && go test -race -count=1 -run Malformed ./tracker/http ./webseed
	cd internal/anacrolix-torrent && go test -race -count=1 -run DecodeDepth ./bencode

## e2e             end-to-end stream test: seed a private torrent, read it through FUSE in the real image (Docker, /dev/fuse)
e2e:
	docker build -f docker/Dockerfile -t flodni:ci .
	docker build -f docker/Dockerfile --target e2e-seeder -t flodni-e2e-seeder:ci .
	PROMTOOL_IMAGE=$(PROMTOOL_IMAGE) bash tests/e2e/run.sh flodni:ci flodni-e2e-seeder:ci

## validate        parse the workspace with the pinned Structurizr image
validate:
	$(STRUCTURIZR) $(STRUCTURIZR_IMAGE) validate -workspace /w/workspace.dsl

## inspect         list model findings; fails on any ERROR line
inspect:
	@out="$$($(STRUCTURIZR) $(STRUCTURIZR_IMAGE) inspect -workspace /w/workspace.dsl 2>&1)"; \
	printf '%s\n' "$$out"; \
	if printf '%s\n' "$$out" | grep -q 'ERROR'; then echo "inspect: errors found" >&2; exit 1; fi

## check           validate + inspect: run before committing a model change
check: validate inspect

## docs            fail when documentation contradicts the tree (links, indexes, ADRs, view register, IDs, headings)
docs:
	python3 scripts/check_docs_consistency.py

## test            unit tests for the checker and the Mermaid and PDF scripts, and the guard-bash case table
test:
	python3 -m unittest discover -s tests
	@out=$$(bash tests/test_guard_bash.sh) || { printf '%s\n' "$$out" | grep -v '^ok'; exit 1; }

## view            browse the workspace at http://localhost:8080/workspace/1 (PORT=... to change; Ctrl-C stops it)
view:
	docker run --rm -p $(PORT):8080 -v "$(CURDIR)/$(ARCH_DIR):/usr/local/structurizr" $(STRUCTURIZR_IMAGE) local

## export          every view as SVG, PNG and Mermaid, plus the workspace JSON, into the generated folder
export:
	mkdir -p $(GENERATED)
	chmod 777 $(GENERATED)
	$(STRUCTURIZR) -v "$(CURDIR)/$(GENERATED):/out" $(STRUCTURIZR_IMAGE) export -workspace /w/workspace.dsl -format json -output /out
	$(STRUCTURIZR) -v "$(CURDIR)/$(GENERATED):/out" $(STRUCTURIZR_IMAGE) export -workspace /w/workspace.dsl -format mermaid -output /out
	$(STRUCTURIZR) -v "$(CURDIR)/$(GENERATED):/out" $(STRUCTURIZR_IMAGE)-playwright export -workspace /w/workspace.dsl -format svg -output /out
	$(STRUCTURIZR) -v "$(CURDIR)/$(GENERATED):/out" $(STRUCTURIZR_IMAGE)-playwright export -workspace /w/workspace.dsl -format png -output /out
	@echo "exported $$(ls $(GENERATED) | wc -l | tr -d ' ') files to $(GENERATED)"

## mermaid-views   refresh the diagrams marked <!-- mermaid-view: Key --> from the model
mermaid-views:
	mkdir -p $(GENERATED)/mermaid-export
	chmod 777 $(GENERATED)/mermaid-export
	rm -f $(GENERATED)/mermaid-export/*.mmd
	$(STRUCTURIZR) -v "$(CURDIR)/$(GENERATED)/mermaid-export:/out" $(STRUCTURIZR_IMAGE) export -workspace /w/workspace.dsl -format mermaid -output /out
	python3 scripts/mermaid_blocks.py sync

## mermaid-render  render every Mermaid block in the Markdown; fails on a syntax error
mermaid-render:
	python3 scripts/mermaid_blocks.py extract
	MERMAID_IMAGE=$(MERMAID_IMAGE) scripts/render-mermaid.sh $(GENERATED)/mermaid-render

## mermaid         mermaid-views, then mermaid-render
mermaid: mermaid-views mermaid-render

## pdf             the Documentation tab and every view as one PDF, named <project>-architecture-<date>-<edition>.pdf
pdf:
	STRUCTURIZR_IMAGE=$(STRUCTURIZR_IMAGE) PANDOC_IMAGE=$(PANDOC_IMAGE) MERMAID_IMAGE=$(MERMAID_IMAGE) ARCH_DIR=$(ARCH_DIR) scripts/architecture-pdf.sh

## clean           delete the generated folder (exports and PDFs; all gitignored)
clean:
	rm -rf $(GENERATED)
