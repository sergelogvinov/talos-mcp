# syntax = docker/dockerfile:1.22
########################################

FROM --platform=${BUILDPLATFORM} golang:1.27.1-alpine AS builder
RUN apk update && apk add --no-cache make
ENV GO111MODULE=on
WORKDIR /src

COPY ["go.mod", "go.sum", "/src/"]
RUN go mod download && go mod verify

COPY . .
ARG VERSION
ARG TAG
ARG SHA
RUN make build-all-archs

########################################

FROM --platform=${TARGETARCH} scratch AS talos-mcp
LABEL org.opencontainers.image.source="https://github.com/sergelogvinov/talos-mcp" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.description="Opinionated MCP server for Proxmox"

COPY --from=gcr.io/distroless/static-debian13:nonroot . .
ARG TARGETARCH
COPY --from=builder /src/bin/talos-mcp-${TARGETARCH} /bin/talos-mcp

ENV LISTEN=0.0.0.0
ENTRYPOINT ["/bin/talos-mcp"]

########################################

FROM --platform=${TARGETARCH} scratch AS gorelease

COPY --from=gcr.io/distroless/static-debian13:nonroot . .
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/talos-mcp /bin/talos-mcp

ENV LISTEN=0.0.0.0
ENTRYPOINT ["/bin/talos-mcp"]
