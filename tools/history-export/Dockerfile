FROM golang:1.25-bookworm AS build

RUN apt-get update \
    && apt-get install -y --no-install-recommends libolm-dev pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go mod tidy \
    && go test ./... \
    && go build -trimpath -ldflags="-s -w" -o /out/matrix-history-export .

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libolm3 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/matrix-history-export /usr/local/bin/matrix-history-export
ENTRYPOINT ["/usr/local/bin/matrix-history-export"]