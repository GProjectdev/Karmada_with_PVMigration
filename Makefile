SHELL := /usr/bin/env bash

IMG ?= ghcr.io/gprojectdev/pv-migration-system:dev
GO ?= go
KUSTOMIZE ?= kubectl kustomize
KUBECTL ?= kubectl

.PHONY: help tidy fmt vet test build docker-build docker-push manifests install-crds install-management install-member install-karmada-rbac install-karmada-ric

help:
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "%-24s %s\n", $$1, $$2}'

tidy: ## Sync Go module dependencies.
	$(GO) mod tidy

fmt: ## Format Go code.
	$(GO) fmt ./...

vet: ## Run go vet.
	$(GO) vet ./...

test: ## Run Go tests.
	$(GO) test ./...

build: ## Build the controller manager locally.
	$(GO) build -o bin/manager ./cmd/manager

docker-build: ## Build the manager image. Override IMG=registry/name:tag.
	docker build -t $(IMG) .

docker-push: ## Push the manager image.
	docker push $(IMG)

manifests: ## Render all Kustomize bundles.
	$(KUSTOMIZE) config/crd >/dev/null
	$(KUSTOMIZE) config/management >/dev/null
	$(KUSTOMIZE) config/member >/dev/null
	$(KUSTOMIZE) config/karmada/rbac >/dev/null
	$(KUSTOMIZE) config/karmada/ric >/dev/null

install-crds: ## Install CRDs into the selected kube context.
	$(KUBECTL) apply -k config/crd

install-management: ## Install hosted management controller into the selected host-cluster context.
	$(KUBECTL) apply -k config/management

install-member: ## Install member controller into the selected member-cluster context.
	$(KUBECTL) apply -k config/member

install-karmada-rbac: ## Install Karmada control-plane RBAC into the selected Karmada context.
	$(KUBECTL) apply -k config/karmada/rbac

install-karmada-ric: ## Install Karmada ResourceInterpreterCustomization into the selected Karmada context.
	$(KUBECTL) apply -k config/karmada/ric
