.PHONY: build build-web image test race vet run check
build:
	mkdir -p bin
	go build -trimpath -o bin/controller ./cmd/controller
	go build -trimpath -o bin/host ./cmd/host
	go build -trimpath -o bin/prepare-env ./internal/cmd/prepare-env
	go build -trimpath -o bin/buildeci ./cmd/buildeci
build-web:
	npm --prefix frontend ci --prefer-offline
	npm --prefix frontend run build
	mkdir -p bin
	go build -tags webui -trimpath -o bin/controller ./cmd/controller
	go build -trimpath -o bin/prepare-env ./internal/cmd/prepare-env
image:
	docker build -f build/Dockerfile.host -t go-harness/host:local .
test:
	go test ./...
race:
	go test -race ./harness/agent ./harness/session ./harness/schedule ./internal/hostruntime ./internal/controller ./internal/config
vet:
	go vet ./...
run:
	./scripts/start.sh
check:
	./scripts/check-dependencies.sh
