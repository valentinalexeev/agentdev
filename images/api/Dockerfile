# agentdev API (#46). Published as ghcr.io/valentinalexeev/agentdev-api (#28).
FROM golang:1.25.14-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /agentdev-api .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /agentdev-api /agentdev-api
# distroless nonroot, numeric so runAsNonRoot can verify it
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/agentdev-api"]
