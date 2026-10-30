# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build

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
