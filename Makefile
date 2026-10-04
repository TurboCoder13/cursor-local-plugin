GOOS := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)
EXT := $(if $(filter darwin,$(GOOS)),dylib,$(if $(filter windows,$(GOOS)),dll,so))
EXE := $(if $(filter windows,$(GOOS)),.exe,)
BUILD_DIR := build/$(GOOS)/$(GOARCH)
PLUGIN := $(BUILD_DIR)/cursor-local.$(EXT)

.PHONY: build check check-go check-deployment host-check install

build:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o $(PLUGIN) .
	go build -trimpath -o $(BUILD_DIR)/cursor-login$(EXE) ./cmd/cursor-login

check: check-go check-deployment

check-go:
	mkdir -p build
	test -z "$$(gofmt -l native.go internal cmd)"
	go vet ./...
	go test -race -coverprofile=build/coverage.out ./internal/... ./cmd/...

check-deployment:
	ruby tools/platform-test.rb

host-check: build
	ruby tools/host-check.rb $(ARGS)

install: build check host-check
	ruby tools/install.rb $(ARGS)
