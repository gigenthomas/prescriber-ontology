# syntax=docker/dockerfile:1.7
#
# Multi-stage build for the prescriber-bot HTTP chatbot.
# Build context: the repo root. The COPY paths below are relative to it
# so the runtime image can pick up metrics.yaml, actions.yaml, queries/,
# and templates that the bot loads at startup.

FROM golang:1.23-alpine AS build
WORKDIR /src
COPY web/go.mod web/go.sum ./web/
RUN --mount=type=cache,target=/go/pkg/mod \
    cd web && go mod download
COPY web ./web
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd web && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/prescriber-bot .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 bot
WORKDIR /app
# Templates are embedded in the binary via go:embed; metrics/actions/queries
# are loaded from the working directory at startup.
COPY --from=build /out/prescriber-bot /usr/local/bin/prescriber-bot
COPY metrics.yaml actions.yaml ./
COPY queries ./queries
USER bot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/prescriber-bot"]
