# agentdev operator (#47). Published as ghcr.io/valentinalexeev/agentdev-operator (#28).
FROM golang:1.25.14-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /agentdev-operator .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /agentdev-operator /agentdev-operator
# distroless nonroot, numeric so runAsNonRoot can verify it
USER 65532:65532
ENTRYPOINT ["/agentdev-operator"]
