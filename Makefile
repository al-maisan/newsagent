.PHONY: build test vet

build:
	CGO_ENABLED=0 go build -trimpath -o newsagent ./cmd/newsagent

test:
	go test -race ./...

vet:
	go vet ./...
