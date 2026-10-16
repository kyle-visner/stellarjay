# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build

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
