FROM node:24-alpine AS assets

WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci --ignore-scripts
COPY web ./web
RUN npm run build:css

FROM --platform=$BUILDPLATFORM golang:1.26.11-alpine AS build

ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=assets /src/web/style.css ./web/style.css
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/raises ./cmd/raises
# Pre-create the SQLite data directory owned by the runtime uid, so a fresh
# named volume mounted at /data inherits writable ownership on first use.
RUN mkdir -p /out/data && chown 65532:65532 /out/data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65532:65532 /out/raises /raises
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME ["/data"]
EXPOSE 8080
# Scratch has no usernames, so this is the numeric "nonroot" uid. The process
# can read the binary and CA certs and write /data. A pre-existing root-owned
# volume needs a one-time chown -R 65532:65532 (see docs/self-hosting.md).
USER 65532:65532
ENTRYPOINT ["/raises"]
