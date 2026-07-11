# syntax=docker/dockerfile:1.6

FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY . .
# COVER is empty for the normal image. The integration-coverage image
# (make -C tracker test-e2e-cover / test/e2e/run-cover.sh) passes
#   --build-arg COVER="-cover -covermode=atomic -coverpkg=github.com/token-bay/token-bay/tracker/..."
# so the binary writes Go coverage counter files to $GOCOVERDIR on
# clean exit (SIGTERM -> graceful shutdown -> exit 0 flushes; SIGKILL
# does not).
ARG COVER=
RUN mkdir -p /out \
 && go work sync \
 && cd tracker \
 && CGO_ENABLED=0 go build $COVER -o /out/token-bay-tracker ./cmd/token-bay-tracker

FROM alpine:3.20
RUN apk add --no-cache ca-certificates sqlite
COPY --from=builder /out/token-bay-tracker /usr/local/bin/token-bay-tracker
RUN mkdir -p /data && chown 1000:1000 /data
# Coverage sink for -cover builds (the regular binary ignores
# GOCOVERDIR). uid-1000 writable so the unprivileged tracker process
# can flush counter files; compose.cover.yaml bind-mounts a host dir
# over it for collection.
ENV GOCOVERDIR=/covdata
RUN mkdir -p /covdata && chown 1000:1000 /covdata
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/token-bay-tracker"]
CMD ["--help"]
