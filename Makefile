# Version comes from git (R24): the base commit is 0.1.0, each commit on main
# adds one patch step.
VERSION := 0.1.$(shell echo $$(( $$(git rev-list --count HEAD) - 1 )))

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
