# ---- build stage ------------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o /out/krabby ./cmd/krabby

# ---- runtime stage ----------------------------------------------------------
FROM python:3.12-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends git openssh-client ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir "graphifyy[mcp]"

COPY --from=build /out/krabby /usr/local/bin/krabby

ENV KRABBY_DATA_DIR=/data
VOLUME /data

EXPOSE 8080

ENTRYPOINT ["krabby"]
