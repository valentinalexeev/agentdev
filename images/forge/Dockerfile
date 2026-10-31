# agentdev forge sidecar (#64): the only container of an agent pod that holds the forge token.
# Published as ghcr.io/valentinalexeev/agentdev-forge (#28).
FROM golang:1.25.14-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod ./
COPY *.go ./
RUN go vet ./... && go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /forge .

# Not distroless: the sidecar runs git (fetch, bundle, push), which needs libc, its helper programs and libcurl.
# Alpine with git and CA certificates only; no shell is ever started by the sidecar, and its git gets no repo- or
# user-controlled config (see git.go).
FROM alpine:3.22
RUN apk add --no-cache git ca-certificates \
 && mkdir -p /forge && chown 1001 /forge
COPY --from=build /forge /usr/local/bin/forge
# numeric, so runAsNonRoot can verify it; the agent container runs as 1000
USER 1001:1001
ENTRYPOINT ["/usr/local/bin/forge"]
