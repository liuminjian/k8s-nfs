NAME=k8s-nfs
OS ?= linux
VERSION ?= test
all: test

publish: compile build

compile:
	@echo "==> Building the project"
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build --ldflags "-extldflags -static" -mod=vendor  -o ./cmd/nfs/k8s-nfs ./cmd/nfs/main.go

test:
	@echo "==> Testing all packages"
	@go test -v ./...

build:
	@echo "==> Building the docker image"
	@docker build --rm -t centos/nfs cmd/centos -f cmd/centos/Dockerfile
	@docker build --rm -t duni/k8s-nfs:$(VERSION) cmd/nfs -f cmd/nfs/Dockerfile

vendor:
	@GO111MODULE=on go mod tidy
	@GO111MODULE=on go mod vendor

.PHONY: vendor build test compile