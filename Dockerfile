# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd cmd
COPY internal internal
# Pure-Go build (modernc SQLite): no cgo, so the binaries run on a static base image.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/liveprobe ./cmd/liveprobe \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/liveprobe-collector ./cmd/liveprobe-collector \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/liveprobe /out/liveprobe-collector /usr/local/bin/
# The image has no shell to mkdir/chown, so the data dir is copied in owned by `nonroot` (65532);
# a fresh named volume inherits that ownership on first mount.
COPY --from=build --chown=65532:65532 /out/data /data

ENV PORT=4319
ENV DB_PATH=/data/liveprobe.db

EXPOSE 4319
VOLUME /data

# No shell or curl in distroless: the binary probes its own /healthz.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/liveprobe", "healthcheck"]

ENTRYPOINT []
CMD ["/usr/local/bin/liveprobe"]
