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
export IMAGE_NAME=$(DOCKER_REPO):$(DOCKER_TAG)
export SOURCE_BRANCH=$(BRANCH)
export SOURCE_COMMIT=$(SHA1)
export SOURCE_TYPE=git
export SOURCE_REPOSITORY_URL=$(ORIGIN)

all: build

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

deps:
	go get .

run-docker: ## Run dockerized service directly
	docker run $(DOCKER_NAME):latest

push: ## docker push image to registry
	docker push $(DOCKER_NAME):latest

build-local: ## Build the project
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build .
	docker build -t $(DOCKER_NAME):latest .

run: ## Build and run the project
	go build . && ./redis-sentinel-proxy

clean:
	-rm -rf build
