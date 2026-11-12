FROM node:22-alpine AS web-builder
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/index.html web/tsconfig.json ./
COPY web/src ./src
RUN npm run build

FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
RUN go test ./cmd/server && CGO_ENABLED=0 GOOS=linux go build -trimpath -o /crossdrop ./cmd/server

FROM alpine:3.22
WORKDIR /app
COPY --from=builder /crossdrop /app/crossdrop
COPY --from=web-builder /web/dist /app/web
RUN mkdir -p /data && chown 10001:10001 /data
ENV DATA_DIR=/data
USER 10001:10001
EXPOSE 8787
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD wget -q --spider http://127.0.0.1:8787/healthz || exit 1
ENTRYPOINT ["/app/crossdrop"]
