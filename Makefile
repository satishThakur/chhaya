.PHONY: test build evals e2e

test:
	go test ./...

build:
	go build -o bin/chhayad ./cmd/chhayad
	go build -o bin/chhayactl ./cmd/chhayactl

evals:
	go test ./internal/policy/... -v

e2e:
	go test -tags e2e ./test/e2e/... -v
