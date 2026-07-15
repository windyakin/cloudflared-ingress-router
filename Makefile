IMG ?= ghcr.io/windyakin/cloudflared-ingress-router:latest

.PHONY: all
all: build

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test: fmt vet
	go test ./... -coverprofile cover.out

.PHONY: build
build: fmt vet
	CGO_ENABLED=0 go build -o bin/cloudflared-ingress-router ./cmd

.PHONY: run
run: fmt vet
	go run ./cmd

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push:
	docker push $(IMG)
