# Copies the pre-built binary (built by goreleaser or `make build`) instead of compiling.
FROM python:3.12-slim

ARG GRAPHIFYY_VERSION=0.9.26

RUN apt-get update \
    && apt-get install -y --no-install-recommends git openssh-client ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir "graphifyy==${GRAPHIFYY_VERSION}"

# goreleaser lays the build context out as <os>/<arch>/krabby, one directory per
# target platform, so the COPY must be platform-qualified for buildx to pick the
# right binary for each entry in the manifest.
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/krabby /usr/local/bin/krabby

ENV KRABBY_DATA_DIR=/data
VOLUME /data

# krabby reads the container's cgroup memory limit at startup and sizes its
# databases, caches and GOMEMLIMIT from it, so run this image with an explicit
# memory limit (docker run -m 4g, or a Kubernetes resources.limits.memory).
# Without one it assumes the host's total memory and tunes accordingly.

EXPOSE 8080

ENTRYPOINT ["krabby"]
