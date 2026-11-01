# agentdev Claude credential proxy (#66): the only component that holds the Claude credential.
# Published as ghcr.io/valentinalexeev/agentdev-credproxy (#28). Vet and tests run in CI (lint job).
FROM --platform=$BUILDPLATFORM golang:1.25.14-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags=-s -o /credproxy .

# distroless static: no shell, no package manager; CA certificates for the upstream TLS
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /credproxy /credproxy
USER 65532:65532
ENTRYPOINT ["/credproxy"]
