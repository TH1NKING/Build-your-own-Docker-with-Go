.PHONY: build check fmt-check shell-check demo-check demo-prepare demo-doctor demo demo-verify test test-migrations test-worker-api test-worker-execution test-sandbox-creation test-sandbox-init test-sandbox-acceptance vet

GO_FILES := $(shell find . -type f -name '*.go' -not -path './.git/*' -not -path './.cache/*')

check: fmt-check vet test build shell-check demo-check

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

test-worker-api:
	@test -n "$$AGENT_TEST_DATABASE_URL" || { echo 'AGENT_TEST_DATABASE_URL must point to a dedicated test PostgreSQL instance'; exit 1; }
	go test ./tests -run '^(TestWorkerCredential|TestWorkerAPI|TestAgentctlWorker|TestControlPlane)' -count=1

test-worker-execution:
	bash tests/run-worker-execution-linux.sh

build:
	go build ./...

shell-check:
	@for script in with_shell/*.sh; do bash -n "$$script" || exit; done
	bash -n tests/run-sandbox-creation-linux.sh
	bash -n tests/run-sandbox-init-linux.sh
	bash -n tests/run-sandbox-acceptance-linux.sh
	bash -n tests/run-sandbox-file-demo-linux.sh
	bash -n tests/run-worker-execution-linux.sh
	bash -n tests/sandbox-cgroup-fixture.sh
	bash -n tests/run-interview-demo-linux.sh
	bash -n tests/run-runtime-lab-demo-linux.sh
	bash -n tests/run-sandbox-demo-linux.sh
	bash -n tests/run-worker-demo-linux.sh
	bash -n tests/sandbox-test-result.sh

demo-check:
	python3 -B -m unittest discover -s tests -p 'test_*.py'
	bash tests/worker-demo-contract-test.sh

demo-prepare:
	bash tests/run-interview-demo-linux.sh prepare

demo-doctor:
	bash tests/run-interview-demo-linux.sh doctor

demo:
	bash tests/run-interview-demo-linux.sh run $(or $(SCENARIO),all)

demo-verify:
	bash tests/run-interview-demo-linux.sh verify

test-sandbox-creation:
	bash tests/run-sandbox-creation-linux.sh

test-sandbox-init:
	bash tests/run-sandbox-init-linux.sh

test-sandbox-acceptance:
	bash tests/run-sandbox-acceptance-linux.sh
