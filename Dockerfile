FROM golang:1.25-bookworm AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath -ldflags="-s -w" -o /out/cursor2api ./src

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --no-create-home cursor2api

WORKDIR /app
COPY --from=build /out/cursor2api /app/cursor2api
COPY schema /app/schema
USER 10001:10001

EXPOSE 3010
ENTRYPOINT ["/app/cursor2api", "/app/config.json"]
