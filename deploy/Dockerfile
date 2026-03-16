# Stage 1: Build frontend
FROM node:20-alpine AS web-builder
WORKDIR /web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Stage 2: Build Go binary
FROM golang:1.25-alpine AS go-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-builder /web/dist ./web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X github.com/clawvisor/clawvisor/pkg/version.Version=${VERSION}" -o clawvisor ./cmd/clawvisor

# Stage 3: Runtime (minimal distroless image)
FROM gcr.io/distroless/static-debian12
COPY --from=go-builder /app/clawvisor /clawvisor
COPY --from=web-builder /web/dist /web/dist
EXPOSE 25297
ENTRYPOINT ["/clawvisor", "server"]
