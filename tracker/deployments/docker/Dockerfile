# syntax=docker/dockerfile:1.6

FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY . .
RUN go work sync \
 && cd tracker \
 && CGO_ENABLED=0 go build -o /out/token-bay-tracker ./cmd/token-bay-tracker

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/token-bay-tracker /usr/local/bin/token-bay-tracker
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/token-bay-tracker"]
CMD ["--help"]
