FROM docker.io/library/node:24.22.1-bookworm-slim@sha256:be8204d4bd327363357f532187b752ff62759c31bedf98e9e18fc08c9cbd605c AS node_toolchain
FROM docker.io/library/golang:1.26.9-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c
ARG DEBIAN_SNAPSHOT=20261101T000000Z
RUN rm -f /etc/apt/sources.list.d/debian.sources && \
    printf 'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/%s bookworm main\ndeb [check-valid-until=no] https://snapshot.debian.org/archive/debian-security/%s bookworm-security main\n' \
      "$DEBIAN_SNAPSHOT" "$DEBIAN_SNAPSHOT" > /etc/apt/sources.list && \
    apt-get -o Acquire::ForceIPv4=true -o Acquire::Retries=3 -o APT::Update::Error-Mode=any update && \
    apt-get -o Acquire::ForceIPv4=true -o Acquire::Retries=3 install --no-install-recommends -y python3 && \
    rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/*
COPY --from=node_toolchain /usr/local/bin/node /usr/local/bin/node
COPY --from=node_toolchain /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/npm
COPY npm-age-guard.cjs /opt/npm-age-policy/npm-age-guard.cjs
RUN node -e 'const d=require("/usr/local/lib/node_modules/npm/node_modules/@npmcli/config/lib/definitions"); if (!d.definitions["min-release-age"]) throw Error("npm lacks min-release-age")' && \
    mkdir -p /opt/npm-age-policy/bin && \
    printf '#!/bin/sh\nexec node /opt/npm-age-policy/npm-age-guard.cjs npm /usr/local/lib/node_modules/npm "$@"\n' > /opt/npm-age-policy/bin/npm && \
    printf '#!/bin/sh\nexec node /opt/npm-age-policy/npm-age-guard.cjs npx /usr/local/lib/node_modules/npm "$@"\n' > /opt/npm-age-policy/bin/npx && \
    chmod 755 /opt/npm-age-policy/bin/* && \
    useradd --uid 1000 --create-home --home-dir /home/agent --shell /bin/bash agent && \
    mkdir -p /workspace && chown 1000:1000 /workspace && \
    command -v gcc && command -v git && command -v python3
ENV HOME=/home/agent \
    PATH="/opt/npm-age-policy/bin:${PATH}" \
    NPM_CONFIG_MIN_RELEASE_AGE=7 \
    GOTOOLCHAIN=local \
    GOMAXPROCS=2 \
    GOFLAGS=-p=2 \
    GOCACHE=/workspace/.container-state/go-build \
    GOPATH=/workspace/.container-state/go \
    GOMODCACHE=/workspace/.container-state/go/pkg/mod
USER 1000:1000
WORKDIR /workspace
