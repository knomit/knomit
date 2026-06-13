# syntax=docker/dockerfile:1
#
# Cloud build of the knomit HTTP server (goal #1: a plain HTTP server deployable
# anywhere). The server requires CGO (mattn/go-sqlite3, sqlite-vec, ONNX
# Runtime, daulet/tokenizers) and three native libraries:
#   - libtokenizers.a   — STATIC, linked at build time (-L dist/lib via
#                         internal/embeddings/cgo_link.go)
#   - libonnxruntime.so — dlopen'd at runtime (ORT_LIB_PATH)
#   - graphqlite.so     — SQLite extension dlopen'd at runtime (GRAPHQLITE_LIB_PATH)
# Embedding model files are downloaded from HuggingFace on first start into
# KNOMIT_HOME (mount a volume to persist them).

# ---- web UI -----------------------------------------------------------------
FROM node:22-slim AS web
WORKDIR /web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ---- go build (CGO) ---------------------------------------------------------
FROM golang:1.25-bookworm AS build
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
# Fetch native libs into dist/lib: libtokenizers.a (build-time static link),
# libonnxruntime.so + graphqlite.so (copied into the runtime image below).
RUN go run ./tools/fetchlibs dist/lib
RUN go build -trimpath -o /out/knomit .

# ---- runtime ----------------------------------------------------------------
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates libstdc++6 libgomp1 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/knomit /usr/local/bin/knomit
COPY --from=build /src/dist/lib/libonnxruntime.so /opt/knomit/lib/libonnxruntime.so
COPY --from=build /src/dist/lib/graphqlite.so      /opt/knomit/lib/graphqlite.so
ENV ORT_LIB_PATH=/opt/knomit/lib/libonnxruntime.so \
    GRAPHQLITE_LIB_PATH=/opt/knomit/lib/graphqlite \
    KNOMIT_HOST=0.0.0.0 \
    KNOMIT_PORT=19278 \
    KNOMIT_HOME=/data
VOLUME ["/data"]
EXPOSE 19278
ENTRYPOINT ["knomit"]
CMD ["serve"]
