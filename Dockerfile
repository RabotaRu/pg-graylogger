ARG GOLANG_VERSION=1.16
ARG ALPINE_VERSION=3.14


FROM golang:${GOLANG_VERSION}-alpine${ALPINE_VERSION} AS builder
RUN apk --no-cache add ca-certificates
RUN apk --no-cache add binutils

# enable Go modules support
ENV GO111MODULE=on
ENV CGO_ENABLED=0

WORKDIR pg_graylogger

COPY go.* *.go ./
RUN go mod download

# Copy src code from the host and compile it
#COPY cmd cmd
#COPY pkg pkg
#COPY main.go ./
RUN go build -a -trimpath -o /pg_graylogger
RUN strip /pg_graylogger

###
FROM scratch
LABEL maintainer="o.marin@rabota.ru"
COPY --from=builder /pg_graylogger /bin/
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
VOLUME /var/log/postgresql
ENTRYPOINT ["/bin/pg_graylogger"]
