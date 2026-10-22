.PHONY: test build evals

test:
	go test ./...

build:
	go build -o bin/chhayad ./cmd/chhayad
	go build -o bin/chhayactl ./cmd/chhayactl

evals:
	go test ./internal/policy/... -run 'TestDecide' -v
