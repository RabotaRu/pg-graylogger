ARG GOLANG_VERSION=1.16
ARG VERSION=0.7.2

FROM golang:${GOLANG_VERSION} AS builder
ARG VERSION
# enable Go modules support
ENV GO111MODULE=on
ENV CGO_ENABLED=0

WORKDIR pg_graylogger

# Copy src code from the host and compile it
COPY go.* *.go ./
RUN go mod download
RUN go build -a -trimpath -ldflags "-X main.Version=$VERSION -w" -o /pg_graylogger

###
FROM scratch
LABEL maintainer="o.marin@rabota.ru"
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /pg_graylogger /bin/
ENTRYPOINT ["/bin/pg_graylogger"]
