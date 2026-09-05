APP := trae-api-proxy
BIN_DIR := bin

.PHONY: fmt test vet build check clean

fmt:
	gofmt -w $$(find cmd internal pkg -name '*.go' -type f)

test:
	go test ./...

vet:
	go vet ./...

build:
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -o $(BIN_DIR)/$(APP) ./cmd/trae-api

check: test vet build

clean:
	rm -rf $(BIN_DIR)
