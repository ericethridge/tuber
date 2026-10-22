# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/tuber ./cmd/tuber

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=builder /out/tuber /tuber

USER 65532:65532
EXPOSE 8080

ENTRYPOINT ["/tuber"]
CMD ["--config=/etc/tuber/config.yaml", "--health-addr=:8080"]
