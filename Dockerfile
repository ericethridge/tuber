# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS builder
WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/tuber ./cmd/tuber

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=builder /out/tuber /tuber

USER 65532:65532
EXPOSE 8080

ENTRYPOINT ["/tuber"]
CMD ["--config=/etc/tuber/config.yaml", "--health-addr=:8080"]
