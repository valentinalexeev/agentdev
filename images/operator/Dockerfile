# agentdev operator (#47). Published as ghcr.io/valentinalexeev/agentdev-operator (#28).
# The build stage runs on the builder's own platform and cross-compiles (GOOS/GOARCH from buildx), so multi-arch
# images need no QEMU emulation for the Go build. Vet and tests run in CI (lint job), not here.
FROM --platform=$BUILDPLATFORM golang:1.25.14-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags=-s -o /agentdev-operator .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /agentdev-operator /agentdev-operator
# distroless nonroot, numeric so runAsNonRoot can verify it
USER 65532:65532
ENTRYPOINT ["/agentdev-operator"]
