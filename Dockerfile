FROM node:20-alpine AS web
WORKDIR /src/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build
FROM golang:1.21-alpine AS go
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -o /out/wbot-server ./cmd/wbot-server
FROM alpine:3.20
RUN adduser -D -u 10001 wbot
WORKDIR /app
COPY --from=go /out/wbot-server /usr/local/bin/wbot-server
COPY --from=web /src/web/dist ./web/dist
COPY profiles ./profiles
USER wbot
ENV WBOT_ADDR=0.0.0.0:8080 WBOT_DATA_ROOT=/var/lib/wbot WBOT_PROFILE=/app/profiles/default.yaml WBOT_WORKSPACE_ROOT=/workspace
VOLUME ["/var/lib/wbot","/workspace"]
EXPOSE 8080
ENTRYPOINT ["wbot-server"]
