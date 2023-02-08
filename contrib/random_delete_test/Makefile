BINARY=e-dnevnik-bot-test
CGO_ENABLED=0

all: build

update:
	go get -u
	go mod tidy

check:
	gomajor list

.PHONY: build
build:
	go build -trimpath -o $(BINARY)
