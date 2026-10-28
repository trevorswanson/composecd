# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/composecd ./cmd/composecd

# The docker CLI image includes the compose plugin; composecd drives both, and
# git and ssh for repositories and ssh:// hosts.
FROM docker:29-cli
RUN apk add --no-cache git openssh-client tini
COPY --from=build /out/composecd /usr/local/bin/composecd
VOLUME /var/lib/composecd
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/sbin/tini", "--", "composecd"]
CMD ["server", "--config", "/etc/composecd/composecd.yaml"]
