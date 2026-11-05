# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/feeder . \
 && mkdir -p /out/data

# ---- runtime ----
# distroless/static includes CA certificates and tzdata, and runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/feeder /feeder
# Pre-create /data owned by the nonroot user so a named volume mounted here is writable.
COPY --from=build --chown=65532:65532 /out/data /data

ENV STATE_PATH=/data/state.json \
    HEALTH_ADDR=:8080
VOLUME ["/data"]
EXPOSE 8080

HEALTHCHECK --interval=60s --timeout=5s --start-period=60s --retries=3 \
  CMD ["/feeder", "-healthcheck"]

ENTRYPOINT ["/feeder"]
