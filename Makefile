IMAGE ?= tc-injector:latest
NAMESPACE ?= tc-injector-system

.PHONY: all build test image deploy undeploy install-crd uninstall-crd tidy clean

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
	docker build -t $(IMAGE) .

## Install the CRD into the cluster.
install-crd:
	kubectl apply -f config/crd/tcinjector.yaml

## Remove the CRD from the cluster.
uninstall-crd:
	kubectl delete -f config/crd/tcinjector.yaml --ignore-not-found

## Deploy RBAC and DaemonSet.
deploy: install-crd
	kubectl apply -f config/deploy/rbac.yaml
	kubectl apply -f config/deploy/daemonset.yaml

## Remove the DaemonSet and RBAC.
undeploy:
	kubectl delete -f config/deploy/daemonset.yaml --ignore-not-found
	kubectl delete -f config/deploy/rbac.yaml --ignore-not-found

## Apply the example TCInjector resource.
sample:
	kubectl apply -f config/samples/tcinjector-example.yaml

## Remove build artifacts.
clean:
	rm -rf bin/
