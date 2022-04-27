ARG GOLANG_VERSION=1.17
ARG VERSION=0.9.2

FROM golang:${GOLANG_VERSION} AS builder
ARG VERSION
# enable Go modules support
ENV GO111MODULE=on
ENV CGO_ENABLED=0

WORKDIR pg_graylogger

# Copy src code from the host and compile it
COPY go.* *.go ./
COPY logfile/ ./logfile/
RUN set -ex && \
    ls -l && \
    go mod tidy && \
    go build -a -trimpath -ldflags "-X main.Version=$VERSION -w" -o /pg_graylogger

###
FROM scratch
LABEL maintainer="o.marin@rabota.ru"
COPY --from=builder /pg_graylogger /bin/
ENTRYPOINT ["/bin/pg_graylogger"]
