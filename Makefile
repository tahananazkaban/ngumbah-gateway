# Compatible with GNU Make 3.81 (macOS) and 4.x (WSL2).
SHELL := /bin/sh
.DEFAULT_GOAL := help

-include .env
export

CONTRACTS_TAG := v0.1.0
CONTRACTS_URL := https://github.com/tahananazkaban/ngumbah-contracts.git\#tag=$(CONTRACTS_TAG)
LINT_IMAGE := golangci/golangci-lint:v2.15.0

.PHONY: help env build run test lint generate generate-check docker-build

help: ## List available commands
	@grep -E '^[a-zA-Z_-]+:.*## ' $(firstword $(MAKEFILE_LIST)) | \
		awk 'BEGIN {FS = ":.*## "}; {printf "  %-15s %s\n", $$1, $$2}'

env: ## Create .env from .env.example if missing
	@[ -f .env ] || { cp .env.example .env && echo "created .env"; }

build: ## Build the binary into bin/
	@go build -o bin/gateway ./cmd/gateway

run: env ## Run with live reload (needs tenant-svc on GATEWAY_TENANT_SVC_ADDR)
	@go tool air

test: ## Run tests (no external services needed)
	@go test ./... -count=1

lint: ## Run golangci-lint in Docker
	@docker run --rm -v "$(CURDIR):/src" -w /src $(LINT_IMAGE) golangci-lint run ./...

generate: ## Regenerate gRPC code (contract $(CONTRACTS_TAG)) and GraphQL code
	@rm -rf gen && buf generate "$(CONTRACTS_URL)"
	@go tool gqlgen generate

generate-check: ## Fail if committed generated code is out of date
	@sh scripts/generate-check.sh

docker-build: ## Build the container image
	@docker build -t ngumbah-gateway:dev .
