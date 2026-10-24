.PHONY: test build evals e2e help \
        dev-doctor dev-certs dev-up dev-down dev-restart dev-status \
        dev-logs dev-audit dev-verify dev-stop dev-resume

# Default target: show what there is, because a Makefile nobody can read is a
# script with extra steps.
help:
	@echo 'build and test'
	@echo '  make test          unit tests, every package'
	@echo '  make evals         the L1 safety evals (internal/policy)'
	@echo '  make e2e           the M0b exit check, end to end'
	@echo '  make build         both binaries into bin/'
	@echo
	@echo 'running the stack locally'
	@echo '  make dev-doctor    check env files, tokens and certs — changes nothing'
	@echo '  make dev-certs     generate the CA and both certificates (once)'
	@echo '  make dev-up        preflight, then build and start both containers'
	@echo '  make dev-status    what is running, plus an mTLS probe from both sides'
	@echo '  make dev-logs      follow both containers'
	@echo '  make dev-audit     tail the audit log as decisions happen'
	@echo '  make dev-verify    walk the audit hash chain from genesis'
	@echo '  make dev-stop      kill switch ON  (via chhayactl, not the bot)'
	@echo '  make dev-resume    kill switch off'
	@echo '  make dev-down      stop both containers'

# Every dev verb is one line here and all the logic is in scripts/dev.sh —
# preflight needs conditionals, which Make recipes handle badly.
dev-doctor:  ; @scripts/dev.sh doctor
dev-certs:   ; @scripts/dev.sh certs
dev-up:      ; @scripts/dev.sh up
dev-down:    ; @scripts/dev.sh down
dev-restart: ; @scripts/dev.sh restart
dev-status:  ; @scripts/dev.sh status
dev-logs:    ; @scripts/dev.sh logs
dev-audit:   ; @scripts/dev.sh audit
dev-verify:  ; @scripts/dev.sh verify
dev-stop:    ; @scripts/dev.sh stop
dev-resume:  ; @scripts/dev.sh resume

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
