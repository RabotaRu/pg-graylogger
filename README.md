# pg_graylogger

Service for exporting postgresql csvlog to graylog server.
Additionaly it supports extracting duration of statement as separate field and
binding it to previous statement in a session and depersonalization (removing exact values from statements)
```
Usage of ./pg_graylogger:
  -depers
    	Depersonalize. Replace sensible information (field values) from query texts
  -graylog-address string
    	Address of graylog in form of server:port (default "localhost:2345")
  -log-dir string
    	Path to postgresql log file in csv format (default "/var/log/postgresql")
  -version
    	Show version
```
