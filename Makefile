BINARY_ARCH ?= arm64

build:
	GOOS=linux GOARCH=$(BINARY_ARCH) go build -o bin/mydocker ./cmd/mydocker

test-container:
	docker run --rm -it --privileged \
		-v $(PWD)/bin:/usr/local/bin \
		-v $(PWD)/rootfs-cache:/rootfs-cache \
		mydocker-vm:latest bash
