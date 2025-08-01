# Variables
APP_NAME 	 := katomik
OUTPUT   	 := $(APP_NAME)
COV_REPORT 	 := coverage.txt
TEST_FLAGS 	 := -v -race -timeout 30s
INSTALL_DIR  := /usr/local/bin
PROFILE_DIR  := ./pprof
PPROF_SERVER := http://localhost:7070

ifeq ($(OS),Windows_NT)
	OUTPUT := $(APP_NAME).exe
endif

# Build the binary
.PHONY: build
build: gen
	CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/$(OUTPUT) main.go

# Install the binary to /usr/local/bin
.PHONY: install
install: build
	@echo "Installing bin/$(OUTPUT) to $(INSTALL_DIR)..."
	@install -m 0755 bin/$(OUTPUT) $(INSTALL_DIR)

# Lint the code
.PHONY: lint
lint:
	golangci-lint run --output.tab.path=stdout

.PHONY: gen
gen:
	go generate ./...

# Run unit tests
.PHONY: test
test:
	go test -v -cover ./...

# Check goreleaser
.PHONY: snapshot
snapshot:
	goreleaser release --skip sign --skip publish --snapshot --clean

# Run tests with coverage
.PHONY: test-cov
test-cov:
	go test -coverprofile=$(COV_REPORT) ./...
	go tool cover -html=$(COV_REPORT)

######################################################################
# Integration tests
######################################################################

.PHONY: test-integ
test-integ: install
	@cd test/integration/k8s && bash 00-setup-kind.sh
	go test -tags=integration -v -count=1 ./test/integration/... | tee test-integ-fast.log
