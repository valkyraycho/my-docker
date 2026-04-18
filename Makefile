BINARY_ARCH ?= arm64

build:
	GOOS=linux GOARCH=$(BINARY_ARCH) go build -o bin/mydocker ./cmd/mydocker

test-container:
	docker run --rm --it --privileged \
		-v $(PWD)/bin:/usr/local/bin \
		ubuntu:24.04 bash
