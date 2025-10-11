BRANCH := $(shell git rev-parse --abbrev-ref HEAD)
SHA1 := $(shell git rev-parse HEAD)
SHORT_SHA1 := $(shell git rev-parse --short HEAD)
ORIGIN := $(shell git remote get-url origin)
DATE := $(date -u +'%Y-%m-%dT%H:%M:%Sz')
VER := $(shell git describe --tags --abbrev=0)
DOCK_REPO := patrickdk/redis-sentinel-proxy

export DOCKERFILE_PATH=Dockerfile
export DOCKER_REPO=$(DOCK_REPO)
export DOCKER_TAG=latest
export GIT_BRANCH=$(BRANCH)
export GIT_SHA1=$(SHA1)
export GIT_TAG=$(SHA1)
export GIT_VERSION=$(VER)
export GIT_VERSION_MAJOR=$(shell echo $(VER) | cut -f1 -d.)
export GIT_VERSION_MINOR=$(shell echo $(VER) | cut -f2 -d.)
export IMAGE_NAME=$(DOCKER_REPO):$(DOCKER_TAG)
export SOURCE_BRANCH=$(BRANCH)
export SOURCE_COMMIT=$(SHA1)
export SOURCE_TYPE=git
export SOURCE_REPOSITORY_URL=$(ORIGIN)

all: buildx

buildx:
	docker buildx build --pull --push --platform linux/amd64,linux/arm64 --build-arg BUILD_GOOS=linux --build-arg BUILD_DATE=${BUILD_DATE} --build-arg BUILD_REF=${GIT_SHORT_SHA1} --build-arg BUILD_VERSION=${GIT_VERSION} --build-arg BUILD_REPO=${BUILD_REPO} --file ${DOCKERFILE_PATH} --tag docker.patrickdk.com/valkey-proxy:${GIT_VERSION} .
	skopeo copy --all docker://docker.patrickdk.com/valkey-proxy:${GIT_VERSION} docker://docker.patrickdk.com/valkey-proxy:${GIT_VERSION_MAJOR}.${GIT_VERSION_MINOR}
	skopeo copy --all docker://docker.patrickdk.com/valkey-proxy:${GIT_VERSION} docker://docker.patrickdk.com/valkey-proxy:${GIT_VERSION_MAJOR}
	skopeo copy --all docker://docker.patrickdk.com/valkey-proxy:${GIT_VERSION} docker://docker.patrickdk.com/valkey-proxy:latest
	skopeo copy --all docker://docker.patrickdk.com/valkey-proxy:${GIT_VERSION} docker://${DOCKER_REPO}:${GIT_VERSION}
	skopeo copy --all docker://docker.patrickdk.com/valkey-proxy:${GIT_VERSION} docker://${DOCKER_REPO}:${GIT_VERSION_MAJOR}.${GIT_VERSION_MINOR}
	skopeo copy --all docker://docker.patrickdk.com/valkey-proxy:${GIT_VERSION} docker://${DOCKER_REPO}:${GIT_VERSION_MAJOR}
	skopeo copy --all docker://docker.patrickdk.com/valkey-proxy:${GIT_VERSION} docker://${DOCKER_REPO}:latest
	
build: export DOCKER_TAG=$(GIT_VERSION)
build: docker

release: export DOCKER_TAG=$(GIT_VERSION)
release: export DOCKER_EXTRATAGS=latest
release: release-publish

docker:
	./hooks/post_checkout
	./hooks/pre_build
	./hooks/build
#	./hooks/push

release-publish:
	./hooks/push

update:
	GOPROXY=direct go get -u
	GOPROXY=direct go mod tidy

deps:
	GOPROXY=direct go mod vendor
#	GOPROXY=direct go get .
#	@docker run --rm -it -e GOPROXY=direct -v "${PWD}:/go/src/github.com/patrickdk77/redis-sentinel-proxy/" \
#			-w /go/src/github.com/patrickdk77/redis-sentinel-proxy/ \
#			golang:alpine3.21 sh -c 'apk --no-cache add git && go mod vendor'

run-docker: ## Run dockerized service directly
	docker run $(DOCKER_REPO):latest

push: ## docker push image to registry
	docker push $(DOCKER_REPO):latest

build-local: ## Build the project
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build .
	docker build -t $(DOCKER_REPO):latest .

run: ## Build and run the project
	go build . && ./redis-sentinel-proxy

clean:
	-rm -rf build
