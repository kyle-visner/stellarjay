# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build

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
