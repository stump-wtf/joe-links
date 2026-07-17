# Stage 1 — CSS builder
FROM node:20-alpine AS css-builder
WORKDIR /build
COPY package.json package-lock.json ./
RUN npm ci
COPY tailwind.config.js postcss.config.js* ./
COPY static/ static/
COPY web/ web/
RUN npm run build

# Stage 2 — Go builder
FROM golang:1.25-alpine AS go-builder
ARG VERSION=dev
ARG COMMIT=unknown
ARG BRANCH=unknown
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=css-builder /build/web/static/css/app.css web/static/css/app.css
RUN PKG="github.com/joestump/joe-links/internal/build" && \
    CGO_ENABLED=0 go build \
      -ldflags="-s -w -X ${PKG}.Version=${VERSION} -X ${PKG}.Commit=${COMMIT} -X ${PKG}.Branch=${BRANCH}" \
      -o joe-links ./cmd/joe-links

# Stage 3 — final
# Pinned base for reproducible builds — never a floating :latest tag.
FROM alpine:3.22
# Non-root runtime user; /data is pre-created and chowned so a fresh named
# volume mounted there inherits ownership the SQLite DSN can write to.
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -H -u 65532 joe && \
    mkdir -p /data && chown joe:joe /data
WORKDIR /app
COPY --from=go-builder /build/joe-links .
USER joe
EXPOSE 8080
# /metrics is the unauthenticated Prometheus endpoint; wget is busybox's.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/metrics || exit 1
CMD ["./joe-links", "serve"]
