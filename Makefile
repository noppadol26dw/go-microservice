.PHONY: build test run

build:
	go build -o bin/app ./app

test:
	go test ./...

run: build
	./bin/app

