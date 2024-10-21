VERSION := $(shell git describe --tags --dirty=-modified --always)

REGISTRY := quay.io/skupper
TAG := v2-latest
PLATFORMS ?= linux/amd64,linux/arm64

LDFLAGS := -X github.com/skupperproject/skupper/pkg/version.Version=${VERSION}
TESTFLAGS := -v -race -short

DOCKER := docker

GOOS ?= linux
GOARCH ?= amd64

all: build-cmd build-config-sync build-controller build-bootstrap build-manifest build-network-console-collector

build-cmd:
	GOOS=${GOOS} GOARCH=${GOARCH} go build -ldflags="${LDFLAGS}"  -o skupper ./cmd/skupper

build-bootstrap:
	GOOS=${GOOS} GOARCH=${GOARCH} go build -ldflags="${LDFLAGS}"  -o bootstrap ./cmd/bootstrap

build-controller:
	go build -ldflags="${LDFLAGS}"  -o controller cmd/controller/main.go cmd/controller/controller.go

build-config-sync:
	GOOS=${GOOS} GOARCH=${GOARCH} go build -ldflags="${LDFLAGS}"  -o config-sync cmd/config-sync/main.go cmd/config-sync/config_sync.go cmd/config-sync/collector.go

build-network-console-collector:
	GOOS=${GOOS} GOARCH=${GOARCH} go build -ldflags="${LDFLAGS}"  -o network-console-collector ./cmd/network-console-collector

build-manifest:
	go build -ldflags="${LDFLAGS}"  -o manifest ./cmd/manifest

build-doc-generator:
	GOOS=${GOOS} GOARCH=${GOARCH} go build -ldflags="${LDFLAGS}"  -o generate-doc ./internal/cmd/generate-doc

images:
	mkdir -p images


DOCKERFILE_TARGETS = Dockerfile.bootstrap Dockerfile.config-sync Dockerfile.controller Dockerfile.network-console-collector

docker-build: $(patsubst Dockerfile.%,docker-build-%,$(DOCKERFILE_TARGETS))
multiarch-oci: $(patsubst Dockerfile.%,multiarch-oci-%,$(DOCKERFILE_TARGETS))
multiarch-push: $(patsubst Dockerfile.%,multiarch-push-%,$(DOCKERFILE_TARGETS))

.PHONY:
docker-build-%: Dockerfile.%
	${DOCKER} build -t "${REGISTRY}/$*:${TAG}" -f $< .

multiarch-oci-%: Dockerfile.% images
	${DOCKER} buildx build \
		"--output=type=oci,dest=$(shell pwd)/images/$*.tar" \
		-t "${REGISTRY}/$*:${TAG}" \
		--platform ${PLATFORMS} \
		-f $< .

multiarch-push-%: Dockerfile.%
	${DOCKER} buildx build \
		--push \
		-t "${REGISTRY}/$*:${TAG}" \
		--platform ${PLATFORMS} \
		-f $< .
format:
	go fmt ./...

generate-client:
	./scripts/update-codegen.sh

force-generate-client:
	FORCE=true ./scripts/update-codegen.sh

vet:
	go vet ./...

.PHONY: test
test:
	go test ${TESTFLAGS} ./...

.PHONY: cover
cover:
	go test ${TESTFLAGS} \
		-cover \
		-coverprofile cover.out \
		./...

clean:
	rm -rf skupper controller config-sync manifest bootstrap network-console-collector images
