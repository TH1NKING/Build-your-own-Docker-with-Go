.PHONY: build check fmt-check shell-check test vet

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

build:
	go build ./...

shell-check:
	bash -n with_shell/*.sh
