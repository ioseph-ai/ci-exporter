# Run golangci-lint locally with the same version CI pins, so no surprises.
GOLANGCI_VERSION := v2.12.2

.PHONY: build vet test lint run

build:
	go build ./...

vet:
	go vet ./...

test:
	go test -race ./...

lint:
	curl -fsSL -o /tmp/glci.tar.gz https://github.com/golangci/golangci-lint/releases/download/$(GOLANGCI_VERSION)/golangci-lint-2.12.2-linux-amd64.tar.gz
	echo "8df580d2670fed8fa984aac0507099af8df275e665215f5c7a2ae3943893a553  /tmp/glci.tar.gz" | sha256sum -c -
	tar -xzf /tmp/glci.tar.gz -C /usr/local/bin --strip-components=1 golangci-lint-2.12.2-linux-amd64/golangci-lint
	golangci-lint run ./...

run:
	MODE=gitlab GITLAB_TOKEN=$${GITLAB_TOKEN:?set GITLAB_TOKEN} go run .
