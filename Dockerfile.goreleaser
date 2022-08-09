ARG GOLANG_VERSION=1.16
ARG ALPINE_VERSION=3.15

### Base image with shell
FROM alpine:${ALPINE_VERSION} as base-release
RUN apk --update --no-cache add ca-certificates && update-ca-certificates

### Scratch with goreleaser
FROM scratch as scratch-goreleaser
COPY --from=base-release /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY pg-graylogger /bin/
ENTRYPOINT ["/bin/pg-graylogger"]
USER 65534
