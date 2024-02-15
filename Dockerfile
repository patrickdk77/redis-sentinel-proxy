ARG BUILD_FROM_PREFIX

FROM ${BUILD_FROM_PREFIX}golang:alpine3.19 AS build
ARG BUILD_ARCH
ARG QEMU_ARCH
COPY .gitignore qemu-${QEMU_ARCH}-static* /usr/bin/
RUN apk --no-cache add gcc musl-dev git
WORKDIR /go/src/
COPY . /go/src/
ARG BUILD_VERSION
ARG BUILD_DATE
ARG BUILD_REF
ARG BUILD_GOARCH
ARG BUILD_GOOS
RUN go mod download \
# && go mod verify \
 && CGO_ENABLED=0 GOOS=${BUILD_GOOS} GOARCH=${BUILD_GOARCH} go build \
    -ldflags '-s -w -X main.ver=${BUILD_VERSION} \
    -X main.commit=${BUILD_REF} -X main.date=${BUILD_DATE}' -o ./health ./healthcheck \
 && CGO_ENABLED=0 GOOS=${BUILD_GOOS} GOARCH=${BUILD_GOARCH} go build \
    -ldflags '-s -w -X main.ver=${BUILD_VERSION} \
    -X main.commit=${BUILD_REF} -X main.date=${BUILD_DATE}' -o .

FROM alpine:3.19 AS libs
RUN apk --no-cache add ca-certificates

FROM scratch
COPY --from=libs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /go/src/redis-sentinel-proxy /redis-sentinel-proxy
COPY --from=build /go/src/health /health
ENTRYPOINT ["/redis-sentinel-proxy"]
HEALTHCHECK --interval=5s --timeout=3s --start-period=15s --retries=3 CMD [ "/health" ]

EXPOSE 6379

#ENV LISTEN_ADDRESS=:6379
#ENV SENTINAL_ADDRESS=sentinal:26379
#ENV REDIS_MASTER_NAME=mymaster
#ENV PASSWORD=xxxx

ARG BUILD_VERSION
ARG BUILD_DATE
ARG BUILD_REF
LABEL maintainer="Patrick Domack (patrickdk@patrickdk.com)" \
  Description="Redis Sentinal Proxy for non-sentinal aware apps" \
  ForkedFrom="" \
  org.label-schema.schema-version="1.0" \
  org.label-schema.build-date="${BUILD_DATE}" \
  org.label-schema.name="redis-sentinel-proxy" \
  org.label-schema.description="Redis Sentinal Proxy for non-sentinal aware apps" \
  org.label-schema.url="https://github.com/patrickdk77/redis-sentinel-proxy" \
  org.label-schema.usage="https://github.com/patrickdk77/redis-sentinel-proxy/tree/master/README.md" \
  org.label-schema.vcs-url="https://github.com/patrickdk77/redis-sentinel-proxy" \
  org.label-schema.vcs-ref="${BUILD_REF}" \
  org.label-schema.version="${BUILD_VERSION}"

