ARG GOLANG_VERSION=1.16


FROM golang:${GOLANG_VERSION} AS builder

# enable Go modules support
ENV GO111MODULE=on
ENV CGO_ENABLED=0

RUN apt-get update
RUN apt-get install busybox-static
WORKDIR pg_graylogger

# Copy src code from the host and compile it
COPY go.* *.go ./
RUN go mod download
RUN go build -a -trimpath -o /pg_graylogger
RUN strip /pg_graylogger

###
FROM scratch
LABEL maintainer="o.marin@rabota.ru"
COPY --from=builder /pg_graylogger /bin/
COPY --from=builder /bin/busybox /bin/
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ENTRYPOINT ["/bin/pg_graylogger"]
