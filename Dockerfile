# Copies the pre-built binary (built by goreleaser or `make build`) instead of compiling.
FROM python:3.12-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends git openssh-client ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir graphifyy

COPY krabby /usr/local/bin/krabby

ENV KRABBY_DATA_DIR=/data
VOLUME /data

EXPOSE 8080

ENTRYPOINT ["krabby"]
