# Version comes from git (R24): the base commit is 0.1.0; patch and minor
# roll over after 99, carrying into the next component.
VERSION := $(shell n=$$(( $$(git rev-list --count HEAD) - 1 + 100 )); printf '%d.%d.%d' $$((n / 10000)) $$((n / 100 % 100)) $$((n % 100)))

.PHONY: build install check

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/germes ./cmd/germes

install:
	go install -ldflags "-X main.version=$(VERSION)" ./cmd/germes

check:
	go build ./...
	go vet ./...
	test -z "$$(gofmt -l .)"
	go test -count=1 ./...
