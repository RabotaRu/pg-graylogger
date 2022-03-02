FROM scratch
LABEL maintainer="o.marin@rabota.ru"
COPY pg-graylogger /bin/
ENTRYPOINT ["/bin/pg-graylogger"]
