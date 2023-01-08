TARGET?=e-dnevnik-bot
CGO_ENABLED?=0

all: update build

update:
	go get -u
	go mod tidy -compat=1.19

check:
	gomajor list

.PHONY: build
build:
	CGO_ENABLED=$(CGO_ENABLED) go build -trimpath -ldflags="-s -w" -o $(TARGET)

.PHONY: build-debug
build-debug:
	CGO_ENABLED=1 go build -race -o $(TARGET)
