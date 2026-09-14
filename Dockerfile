FROM golang:1.27.1-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/kedify/kedr/internal/cli.Version=${VERSION}" -o /out/kedr ./cmd/kedr

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=builder /out/kedr /usr/local/bin/kedr
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/kedr"]
CMD ["simple"]
