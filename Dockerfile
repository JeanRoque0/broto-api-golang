# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates && \
    wget -q https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem -O /tmp/rds.pem && \
    cat /tmp/rds.pem >> /etc/ssl/certs/ca-certificates.crt && \
    mkdir -p /runtime/data/photos && chown -R 10001:10001 /runtime
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -o /broto-api ./cmd/broto-api

FROM scratch AS production
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /broto-api /broto-api
COPY --from=build --chown=10001:10001 /runtime/ /
USER 10001:10001
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD ["/broto-api", "healthcheck"]
ENTRYPOINT ["/broto-api"]
