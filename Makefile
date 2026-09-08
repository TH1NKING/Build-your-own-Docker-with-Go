.PHONY: build check fmt-check shell-check test test-migrations test-sandbox-creation test-sandbox-init vet

GO_FILES := $(shell find . -type f -name '*.go' -not -path './.git/*' -not -path './.cache/*')

check: fmt-check vet test build shell-check

fmt-check:
	@unformatted="$$(gofmt -l $(GO_FILES))"; \
	if [ -n "$$unformatted" ]; then \
		echo "The following Go files need gofmt:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

vet:
	go vet ./...

test:
	go test ./...

test-migrations:
	@test -n "$$AGENT_TEST_DATABASE_URL" || { echo 'AGENT_TEST_DATABASE_URL must point to a dedicated test PostgreSQL instance'; exit 1; }
	go test ./tests -run '^TestAgentctlMigration' -count=1

build:
	go build ./...

shell-check:
	bash -n with_shell/*.sh
	bash -n tests/run-sandbox-creation-linux.sh
	bash -n tests/run-sandbox-init-linux.sh
	bash -n tests/sandbox-cgroup-fixture.sh

test-sandbox-creation:
	bash tests/run-sandbox-creation-linux.sh

test-sandbox-init:
	bash tests/run-sandbox-init-linux.sh
