PKG := github.com/guggero/nearby-pay-req

# The formatting and lint tools are pinned in go.mod's tool block, so every
# checkout runs the same versions. GOWORK=off keeps a workspace file of an
# enclosing project from changing what `go tool` resolves.
GOTOOL := GOWORK=off go tool
GOIMPORTS_PKG := github.com/rinchsan/gosimports/cmd/gosimports
GOLINT_PKG := github.com/golangci/golangci-lint/v2/cmd/golangci-lint

# Every Go file of both modules (the library and the vector generator).
GOFILES := $(shell find . -type f -name '*.go' -not -path './.git/*')

TEST_FLAGS := -count=1 -timeout=2m

# The race detector is on by default. Pass norace=1 only to bisect whether
# a failure is race-detector induced, never as a routine escape hatch.
ifeq ($(norace),)
TEST_FLAGS += -race
endif

# pkg narrows unit tests to one package (e.g. pkg=session), case to one test
# (e.g. case=TestSpecVectors).
ifneq ($(pkg),)
UNITPKG := $(PKG)/$(pkg)
else
UNITPKG := ./...
endif
ifneq ($(case),)
TESTCASE := -run '^$(case)$$'
endif

UNIT := go test $(TEST_FLAGS) $(TESTCASE) $(UNITPKG)

GREEN := "\033[0;32m"
NC := "\033[0m"
define print
	echo $(GREEN)$1$(NC)
endef

.PHONY: all build unit unit-norace fuzz vectors fmt fmt-check fmt-verify lint \
	tidy check

all: check

#? build: Compile every package of both modules
build:
	@$(call print, "Building.")
	go build ./...
	cd spec/gen && go build -o /dev/null .

#? unit: Run the unit tests with the race detector (pkg=..., case=..., norace=1)
unit:
	@$(call print, "Running unit tests.")
	$(UNIT)

#? fuzz: Fuzz the NDEF and chunk decoders for a short while each
fuzz:
	go test -run '^$$' -fuzz FuzzDecodeURIMessage -fuzztime 30s ./ndef
	go test -run '^$$' -fuzz FuzzReassembler -fuzztime 30s ./wire

#? vectors: Regenerate spec/vectors.json with the independent generator
vectors:
	cd spec/gen && go run . > ../vectors.json

#? fmt: Fix imports, gofmt -s and goformat every Go file in place
fmt:
	@$(call print, "Fixing imports.")
	$(GOTOOL) $(GOIMPORTS_PKG) -w $(GOFILES)
	@$(call print, "Formatting source.")
	gofmt -l -w -s $(GOFILES)
	@$(call print, "Formatting using goformat.")
	$(GOTOOL) goformat -w --optimize .

#? fmt-check: Run fmt and fail if it changed anything
fmt-check: fmt
	@$(call print, "Checking fmt results.")
	if test -n "$$(git status --porcelain)"; then echo "code not formatted correctly, please run `make fmt` again!"; git status; git diff; exit 1; fi

#? fmt-verify: List files fmt would change, without changing them
fmt-verify:
	@$(call print, "Verifying formatting.")
	@set -e; \
	unformatted=$$( \
		$(GOTOOL) $(GOIMPORTS_PKG) -l $(GOFILES); \
		gofmt -l -s $(GOFILES); \
		$(GOTOOL) goformat -l --optimize . \
	); \
	if [ -n "$$unformatted" ]; then \
		echo "Not formatted, run 'make fmt':"; \
		echo "$$unformatted" | sed -e 's|^\./||' | LC_ALL=C sort -u \
			| sed -e 's|^|  |'; \
		exit 1; \
	fi

#? lint: Run go vet and golangci-lint on both modules
lint:
	@$(call print, "Linting Go files.")
	go vet ./...
	cd spec/gen && go vet ./...
	$(GOTOOL) $(GOLINT_PKG) run ./...
	cd spec/gen && $(GOTOOL) -modfile=../../go.mod $(GOLINT_PKG) run ./...

#? tidy: Tidy both modules
tidy:
	go mod tidy
	cd spec/gen && go mod tidy

#? check: Everything CI runs
check: build fmt-verify lint unit

#? help: List the targets
help:
	@grep -h '^#? ' $(MAKEFILE_LIST) | sed -e 's/^#? //' | sort
