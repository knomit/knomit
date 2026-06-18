# syntax=docker/dockerfile:1
#
# Cloud build of the knomit HTTP server (goal #1: a plain HTTP server deployable
# anywhere). The image is fully self-contained: ALL dependencies — the native
# libraries AND the embedding model — are fetched at BUILD time. The running
# container performs NO network downloads at startup.
#
# The server requires CGO (mattn/go-sqlite3, sqlite-vec, ONNX Runtime,
# daulet/tokenizers) and three native libraries:
#   - libtokenizers.a   — STATIC, linked at build time (-L dist/lib via
#                         internal/embeddings/cgo_link.go)
#   - libonnxruntime.so — dlopen'd at runtime (ORT_LIB_PATH)
#   - graphqlite.so     — SQLite extension dlopen'd at runtime (GRAPHQLITE_LIB_PATH)
# The embedding model files are downloaded at build time by `knomit warm-models`
# and baked into the image at /data/models.

# ---- web UI -----------------------------------------------------------------
FROM node:22-slim AS web
WORKDIR /web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ---- go build (CGO) ---------------------------------------------------------
FROM golang:1.26-bookworm AS build
# g++ for the tokenizers/ORT C++ link; libsqlite3-dev provides sqlite3.h, which
# the sqlite-vec cgo bindings include at compile time.
RUN apt-get update && apt-get install -y --no-install-recommends g++ libsqlite3-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
ENV CGO_ENABLED=1
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /web/dist ./web/dist
# Fetch native libs into the per-platform dir (dist/<goos>-<goarch>/lib, the
# fetchlibs default and the path cgo_link_linux_<arch>.go statically links
# libtokenizers.a from). Stage the runtime .so files at a fixed /out/lib so the
# runtime stage's COPY is arch-independent.
RUN go run ./tools/fetchlibs
RUN go build -trimpath -o /out/knomit .
RUN mkdir -p /out/lib \
    && cp dist/linux-*/lib/libonnxruntime.so /out/lib/ \
    && cp dist/linux-*/lib/graphqlite.so      /out/lib/
# Bake the embedding model into the image so the runtime never downloads at
# startup. warm-models reuses the real model registry/config and does NOT
# initialise ONNX Runtime, so it runs without the ORT shared library loaded.
RUN KNOMIT_HOME=/seed /out/knomit warm-models

# ---- runtime ----------------------------------------------------------------
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates libstdc++6 libgomp1 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/knomit /usr/local/bin/knomit
COPY --from=build /out/lib/libonnxruntime.so /opt/knomit/lib/libonnxruntime.so
COPY --from=build /out/lib/graphqlite.so      /opt/knomit/lib/graphqlite.so
# Embedding model, pre-downloaded at build time → no startup network access.
COPY --from=build /seed/models /data/models
ENV ORT_LIB_PATH=/opt/knomit/lib/libonnxruntime.so \
    GRAPHQLITE_LIB_PATH=/opt/knomit/lib/graphqlite \
    KNOMIT_HOST=0.0.0.0 \
    KNOMIT_PORT=19278 \
    KNOMIT_HOME=/data
# NOTE: models are baked into the image at /data/models. For a persistent KB,
# mount a NAMED volume at /data (Docker auto-populates it from the image,
# preserving the baked model). A host bind-mount over /data would hide the
# baked model — pre-seed it or use a named volume.
EXPOSE 19278
ENTRYPOINT ["knomit"]
CMD ["serve"]
