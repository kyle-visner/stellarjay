# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build

ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -trimpath -ldflags="-s -w" -o /out/stellarjay-server ./cmd/stellarjay-server
RUN mkdir -p /out/data /out/backups

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65532:65532 /out/data /var/lib/stellarjay
COPY --from=build --chown=65532:65532 /out/backups /var/backups/stellarjay
COPY --from=build /out/stellarjay-server /stellarjay-server
# Pre-rename entrypoint name, kept for one release so existing compose files start.
COPY --from=build /out/stellarjay-server /jaybase-server
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/stellarjay-server"]
CMD ["serve"]
