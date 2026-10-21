# syntax=docker/dockerfile:1
#
# Headless node image (~18 MB). Build, configure once, then run:
#   docker build -t morcore .
#   docker run --rm -it -v morcore:/data -e MORCORE_PASSWORD=... morcore init --yes \
#       --network mainnet --listen 0.0.0.0:8082 --api-key my-agent --password-env MORCORE_PASSWORD
#   docker run -d --stop-timeout 150 -v morcore:/data -e MORCORE_PASSWORD=... -p 127.0.0.1:8082:8082 morcore

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/rcondron/Morpheus-Core-Node/internal/buildinfo.Version=${VERSION}" \
      -o /out/morcore ./cmd/morcore \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/morcore /usr/local/bin/morcore
COPY --from=build --chown=65532:65532 /out/data /data
ENV MORCORE_HOME=/data
VOLUME ["/data"]
# 8082: agent API (keep private) · 3333: provider MOR-RPC (public on provider nodes)
EXPOSE 8082 3333
ENTRYPOINT ["/usr/local/bin/morcore"]
CMD ["start", "--headless"]
