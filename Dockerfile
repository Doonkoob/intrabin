# syntax=docker/dockerfile:1.7
# ---- web UI ----
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/apps/web
COPY apps/web/package.json apps/web/package-lock.json ./
# Optional corporate/proxy CA: docker build --secret id=extra_ca,src=ca.pem
RUN --mount=type=secret,id=extra_ca,required=false \
    if [ -f /run/secrets/extra_ca ]; then export NODE_EXTRA_CA_CERTS=/run/secrets/extra_ca; fi; \
    npm ci --no-audit --no-fund
COPY apps/web/ ./
RUN mkdir -p /src/internal/web && npx tsc -b && npx vite build

# ---- Go binary ----
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=extra_ca,required=false \
    if [ -f /run/secrets/extra_ca ]; then cat /etc/ssl/certs/ca-certificates.crt /run/secrets/extra_ca > /tmp/ca.pem; export SSL_CERT_FILE=/tmp/ca.pem; fi; \
    go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=web /src/internal/web/dist/ internal/web/dist/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/intrabin/intrabin/internal/node.Version=${VERSION} -X github.com/intrabin/intrabin/internal/node.Commit=${COMMIT} -X github.com/intrabin/intrabin/internal/node.BuildDate=${BUILD_DATE}" \
    -o /out/intrabin ./cmd/intrabin
RUN mkdir -p /out/data

# ---- runtime ----
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="Intrabin" \
      org.opencontainers.image.description="Self-hosted distributed uptime monitoring and recovery automation" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/intrabin/intrabin"
COPY --from=build /out/intrabin /usr/local/bin/intrabin
COPY --from=build --chown=65532:65532 /out/data /var/lib/intrabin
ENV INTRABIN_DATA_DIR=/var/lib/intrabin \
    INTRABIN_LISTEN=:8080
USER 65532:65532
EXPOSE 8080 7443
VOLUME ["/var/lib/intrabin"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/usr/local/bin/intrabin", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/intrabin"]
CMD ["serve"]
