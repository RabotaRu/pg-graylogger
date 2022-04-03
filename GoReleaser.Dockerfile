FROM scratch
COPY pg-graylogger /bin/
ENTRYPOINT ["/bin/pg-graylogger"]
