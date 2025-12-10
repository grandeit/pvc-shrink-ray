FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o pvc-shrink-ray .

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /app/pvc-shrink-ray .
USER 65532:65532
ENTRYPOINT ["/pvc-shrink-ray"]
