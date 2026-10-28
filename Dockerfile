# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/composecd ./cmd/composecd

# SOPS stays a separate CLI dependency; verify a pinned release for each image
# architecture before copying it into the runtime image.
FROM --platform=$BUILDPLATFORM alpine:3 AS sops
ARG TARGETARCH
RUN apk add --no-cache ca-certificates
RUN case "$TARGETARCH" in \
      amd64) checksum=e5bec3346a873ae91d871550f3e698c1aad962aff462a080e40f25fde17fef6b ;; \
      arm64) checksum=53b0abacd38ef1b12a66d6c100956691b9cefce018d91f81e73ddf7438b94d77 ;; \
      *) echo "unsupported SOPS architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac && \
    wget -q -O /sops "https://github.com/getsops/sops/releases/download/v3.13.3/sops-v3.13.3.linux.${TARGETARCH}" && \
    echo "${checksum}  /sops" | sha256sum -c - && chmod 0755 /sops

# The docker CLI image includes the compose plugin; composecd drives both, and
# git and ssh for repositories and ssh:// hosts.
FROM docker:29-cli
RUN apk add --no-cache git openssh-client tini
COPY --from=build /out/composecd /usr/local/bin/composecd
COPY --from=sops /sops /usr/local/bin/sops
COPY third_party/sops/LICENSE /usr/share/licenses/sops/LICENSE
COPY NOTICE /usr/share/doc/composecd/NOTICE
VOLUME /var/lib/composecd
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/sbin/tini", "--", "composecd"]
CMD ["server", "--config", "/etc/composecd/composecd.yaml"]
