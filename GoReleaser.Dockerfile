FROM scratch
COPY pg-graylogger /bin/pg-graylogger
ENTRYPOINT ['/bin/pg-graylogger']
