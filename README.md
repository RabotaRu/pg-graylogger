# pg-graylogger

Service for exporting postgresql `csvlog` to graylog server.
Additionally, it supports depersonalization (removing exact values from statements) and
extracting duration of statement as separate field, binding it to previous statement in a session. 

## Preparation
First of all, you need to enable logging in CSV format in Postgresql.
To do that, open `/etc/postgresql/14/main/postgresql.conf` and add or change lines
```
log_destination = "csvlog"
loggin_collector = on
```

## Installation

### Deploy with Docker

You just need to bind mount directory where postgresql write logs with CSV format from host to `/var/log/postgresql` inside container and set `-graylog-address` so it knows where to send logs.

```bash
docker run -v "/var/log/postgresql:/var/log/postgresql" pg_graylogger:latest -graylog-address graylog.local:2345 -depers -facility develop
```

## Usage

```console
Usage of ./pg_graylogger:
  -cache-size int
    	ReadAhead buffer cache size (default 10)
  -compressioin-type string
    	Compression type (gzip, zlib or none) (default "gzip")
  -compression-level int
    	Compression level for gelf packets (default 5)
  -depers
    	Depersonalize. Replace sensible information (field values) from query texts
  -facility string
    	Facility field for log messages
  -gelf-streams int
    	Number of UDP GELF forming streams (default 1)
  -graylog-address string
    	Address of graylog in form of server:port (default "localhost:2345")
  -log-dir string
    	Path to postgresql log file in csv format (default "/var/log/postgresql")
  -processing-threads int
    	Number of record-processing threads (default 1)
  -version
    	Show version
```

