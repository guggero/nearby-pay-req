PKG := github.com/guggero/nearby-pay-req

GOTEST := go test -timeout=2m

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

.PHONY: all build unit unit-race fuzz vectors lint fmt tidy check

all: check

#? build: Compile every package
build:
	go build ./...

#? unit: Run the unit tests (pkg=..., case=...)
unit:
	$(GOTEST) -count=1 $(TESTCASE) $(UNITPKG)

#? unit-race: Run the unit tests with the race detector
unit-race:
	$(GOTEST) -race -count=1 $(TESTCASE) $(UNITPKG)

#? fuzz: Fuzz the NDEF and chunk decoders for a short while each
fuzz:
	go test -run '^$$' -fuzz FuzzDecodeURIMessage -fuzztime 30s ./ndef
	go test -run '^$$' -fuzz FuzzReassembler -fuzztime 30s ./wire

#? vectors: Regenerate spec/vectors.json with the independent generator
vectors:
	cd spec/gen && go run . > ../vectors.json

#? lint: Run golangci-lint
lint:
	golangci-lint run ./...

#? fmt: Format all Go sources
fmt:
	gofmt -s -w .

#? tidy: Tidy both modules
tidy:
	go mod tidy
	cd spec/gen && go mod tidy

#? check: Everything CI runs
check: build lint unit-race
