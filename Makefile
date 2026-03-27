IMAGE ?= quay.io/manabu.ori/tc-injector:v1
NAMESPACE ?= tc-injector-system

.PHONY: all build test image deploy undeploy install-crd uninstall-crd install-scc uninstall-scc tidy clean

all: build

## Download and tidy go modules.
tidy:
	go mod tidy

## Run all unit tests.
test: tidy
	go test ./pkg/... -v -count=1

## Run tests with race detector.
test-race: tidy
	go test ./pkg/... -v -count=1 -race

## Build the binary for the local OS.
build: tidy
	go build -o bin/tc-injector ./cmd/

## Build the container image.
image:
	podman build -t $(IMAGE) .
## Push the container image.
push:
	podman push $(IMAGE)

## Install the CRD into the cluster.
install-crd:
	kubectl apply -f config/crd/tcinjector.yaml

## Remove the CRD from the cluster.
uninstall-crd:
	kubectl delete -f config/crd/tcinjector.yaml --ignore-not-found

## Install the SCC and its ServiceAccount binding (OpenShift only).
install-scc:
	kubectl apply -f config/deploy/scc.yaml
	kubectl apply -f config/deploy/scc-binding.yaml

## Remove the SCC and its binding (OpenShift only).
uninstall-scc:
	kubectl delete -f config/deploy/scc-binding.yaml --ignore-not-found
	kubectl delete -f config/deploy/scc.yaml --ignore-not-found

## Deploy RBAC and DaemonSet.
deploy: install-crd
	kubectl apply -f config/deploy/namespace.yaml
	kubectl apply -f config/deploy/rbac.yaml
	kubectl apply -f config/deploy/daemonset.yaml

## Deploy RBAC, DaemonSet, and SCC (OpenShift).
deploy-openshift: install-crd install-scc
	kubectl apply -f config/deploy/namespace.yaml
	kubectl apply -f config/deploy/rbac.yaml
	kubectl apply -f config/deploy/daemonset.yaml

## Remove the DaemonSet and RBAC.
undeploy:
	kubectl delete -f config/deploy/daemonset.yaml --ignore-not-found
	kubectl delete -f config/deploy/rbac.yaml --ignore-not-found
	kubectl delete -f config/deploy/namespace.yaml --ignore-not-found

## Remove the DaemonSet, RBAC, and SCC (OpenShift).
undeploy-openshift: uninstall-scc
	kubectl delete -f config/deploy/daemonset.yaml --ignore-not-found
	kubectl delete -f config/deploy/rbac.yaml --ignore-not-found
	kubectl delete -f config/deploy/namespace.yaml --ignore-not-found

## Remove build artifacts.
clean:
	rm -rf bin/
