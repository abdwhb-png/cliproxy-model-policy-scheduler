PLUGIN_ID := cliproxy-model-policy-scheduler
OUTPUT := dist/$(PLUGIN_ID).so

.PHONY: fmt test race verify build

fmt:
	gofmt -w *.go

test:
	go test ./...

race:
	go test -race ./...

verify: fmt test race
	go mod verify

build:
	mkdir -p dist
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildmode=c-shared -o $(OUTPUT) .
