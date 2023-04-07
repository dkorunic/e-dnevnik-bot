TARGET?=e-dnevnik-bot
CGO_ENABLED?=0

GIT_LAST_TAG=$(shell git describe --abbrev=0 --tags)
GIT_HEAD_COMMIT=$(shell git rev-parse --short HEAD)
GIT_TAG_COMMIT=$(shell git rev-parse --short ${GIT_LAST_TAG})
GIT_MODIFIED1=$(shell git diff "${GIT_HEAD_COMMIT}" "${GIT_TAG_COMMIT}" --quiet || echo .dev)
GIT_MODIFIED2=$(shell git diff --quiet || echo .dirty)
GIT_MODIFIED=${GIT_MODIFIED1}${GIT_MODIFIED2}
BUILD_DATE=$(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

all: update build

update:
	go get -u
	go mod tidy

check:
	gomajor list

.PHONY: build
build:
	CGO_ENABLED=$(CGO_ENABLED) go build -trimpath -ldflags="-s -w -extldflags '-static' -X main.GitTag=${GIT_LAST_TAG} -X main.GitCommit=${GIT_HEAD_COMMIT} -X main.GitDirty=${GIT_MODIFIED} -X main.BuildTime=${BUILD_DATE}" -o $(TARGET)

.PHONY: build-debug
build-debug:
	CGO_ENABLED=1 go build -ldflags="-X main.GitTag=${GIT_LAST_TAG} -X main.GitCommit=${GIT_HEAD_COMMIT} -X main.GitDirty=${GIT_MODIFIED} -X main.BuildTime=${BUILD_DATE}" -race -o $(TARGET)

