IMG ?= ghcr.io/willuhmjs/backshots:0.1.0
CHART := charts/backshots

.PHONY: all build test lint manifests crds-chart docker-build helm-lint helm-template

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/backshots ./cmd

test:
	go test ./...

lint:
	go vet ./...

# Generate CRDs into config/crd and sync them into the chart.
manifests:
	go tool controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd
	$(MAKE) crds-chart

# Helm installs crds/ only on first install and never upgrades it.
# After changing the API, run this, then `kubectl apply -f charts/backshots/crds/` by hand.
crds-chart:
	mkdir -p $(CHART)/crds
	cp config/crd/*.yaml $(CHART)/crds/

docker-build:
	docker build -t $(IMG) .

helm-lint:
	helm lint $(CHART)

helm-template:
	helm template backshots $(CHART) --namespace backshots \
	  --set store.create=true --set store.nfs.server=192.168.1.5 --set store.nfs.path=/volume1/k8s-backup
