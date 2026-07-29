# syntax=docker/dockerfile:1

# ---- build ----
#
# Pinned to BUILDPLATFORM and cross-compiled via GOARCH, rather than letting buildx
# run an emulated arm64 toolchain: the binary is CGO-free, so cross-compiling is
# both correct and minutes faster than QEMU.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Dependencies first so they cache independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/forwardlimit ./cmd/forwardlimit

# ---- runtime ----
# distroless static: no shell, no package manager, non-root by default.
FROM gcr.io/distroless/static:nonroot

COPY --from=build /out/forwardlimit /forwardlimit

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/forwardlimit"]
