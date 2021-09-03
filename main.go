package main

import (
	"bytes"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/Graylog2/go-gelf.v2/gelf"
	"k8s.io/utils/inotify"
)

const (
	Version       = "v0.6.0"
	bufSize       = 1024 * 1024
	logTimeFormat = "2006-01-02 15:04:05.000 MST"
	shortMsgLen   = 100
	nanoSec       = 1000000000.0
	maxStmtLen    = 7 * 1024
)

var (
	showVer, depersonalize                  bool
	debug, logDir, graylogAddress, facility string
	cacheSize                               int
	pgCsvLogFields                          = [...]string{
		"log_time",
		"user_name",
		"database_name",
		"process_id",
		"connection_from",
		"session_id",
		"session_line_num",
		"command_tag",
		"session_start_time",
		"virtual_transaction_id",
		"transaction_id",
		"error_severity",
		"sql_state_code",
		"message",
		"detail",
		"hint",
		"internal_query",
		"internal_query_pos",
		"context",
		"query",
		"query_pos",
		"location",
		"application_name",
		"backend_type",
	}
)

func main() {
	flag.StringVar(&graylogAddress, "graylog-address", "localhost:2345",
		"Address of graylog in form of server:port")
	flag.StringVar(&logDir, "log-dir", "/var/log/postgresql",
		"Path to postgresql log file in csv format")
	flag.IntVar(&cacheSize, "cache-size", 10, "ReadAhead buffer cache size (default: 10)")
	flag.StringVar(&facility, "facility", "", "Facility field for log messages")
	flag.BoolVar(&depersonalize, "depers", false,
		"Depersonalize. Replace sensible information (field values) from query texts")
	flag.BoolVar(&showVer, "version", false, "Show version")
	flag.Parse()

	if showVer {
		fmt.Println(Version)
		os.Exit(0)
	}

	if debug = os.Getenv("DEBUG"); debug != "" {
		go func() { log.Println(http.ListenAndServe(debug, nil)) }()
	}

	preprocChan := make(chan []string)
	graylogChan := make(chan map[string]interface{})
	errChan := make(chan error)

	go graylogWriter(graylogAddress, graylogChan, errChan)
	go rowsPreproc(preprocChan, graylogChan, errChan)
	go csvLogReader(logDir, preprocChan, errChan)

	var ok bool
	err, ok := <-errChan
	if ok && err != nil {
		log.Fatalln(err)
	}
}

func csvLogReader(logDir string, rowChan chan<- []string, errChan chan<- error) {
	defer close(rowChan)
	var log_file *logFile
	defer log_file.Close()
	var skipedfirst bool
	var csvReader *csv.Reader
	var watcher *inotify.Watcher
	watcher, err := inotify.NewWatcher()
	if err != nil {
		errChan <- fmt.Errorf("error creating intofiy.Watcher: %w", err)
		return
	}
	defer watcher.Close()
	for {
		var event *inotify.Event
		if err = watcher.AddWatch(logDir, inotify.InModify); err != nil {
			errChan <- fmt.Errorf("error adding %v path to inotify.Watcher: %w", logDir, err)
			return
		}
		select {
		case event = <-watcher.Event:
			if !strings.HasSuffix(event.Name, ".csv") {
				continue
			}
			// Workaround for one weird event after log file InCloseWrite
			if log_file != nil && event.Name == log_file.Name() && !skipedfirst {
				skipedfirst = true
				continue
			}
			skipedfirst = false
		case err = <-watcher.Error:
			errChan <- fmt.Errorf("inotify watch error: %w", err)
			return
		}

		log_file, err = OpenLogFile(event.Name, cacheSize)
		if err != nil {
			errChan <- fmt.Errorf("error open log file: %w", err)
			return
		}
		if csvReader == nil {
			if _, err = log_file.Seek(0, io.SeekEnd); err != nil {
				errChan <- fmt.Errorf("error seek to end of log file: %w", err)
			}
		}
		log.Println("Begin reading log file:", event.Name)
		csvReader = csv.NewReader(log_file)
		csvReader.FieldsPerRecord = 0

		if err = watcher.RemoveWatch(logDir); err != nil {
			errChan <- fmt.Errorf("error temporary removing %v path from inotify.Watcher: %w", logDir, err)
			return
		}

		for {
			row, err := csvReader.Read()
			if errors.Is(err, io.EOF) {
				log_file.Close()
				runtime.GC()
				break
			}
			if err != nil {
				errChan <- fmt.Errorf("error reading next row: %w", err)
				return
			}
			rowChan <- row
		}
	}
}

func rowsPreproc(rowChan <-chan []string,
	gelfChan chan<- map[string]interface{},
	errChan chan<- error) {
	defer close(gelfChan)
	var err error
	statementsCache := make(map[interface{}]string, 1024)
	reMsg := regexp.MustCompile("(?is)" +
		`^(?:duration:\s(?P<duration>\d+\.\d{3})\sms\s*|)` +
		`(?:(?:statement|execute .+?):\s*(?P<statement>.*?)\s*|)$`)
	reDepers := regexp.MustCompile(`(?is)(VALUES|IN)\s*\((.*?)\)`)

	for row := range rowChan {
		rowMap := make(map[string]interface{}, 32)
		for index, value := range row {
			switch pgCsvLogFields[index] {
			case "log_time":
				rowMap["log_time"], err = time.Parse(logTimeFormat, value)
				if err != nil {
					errChan <- fmt.Errorf("coundn't parse log time: %w", err)
					return
				}
			case "message":
				if depersonalize {
					value = reDepers.ReplaceAllString(value, "$1 ( DEPERSONALIZED )")
				}
				if vs := len(value); vs > maxStmtLen {
					value = value[:maxStmtLen]
					rowMap["huge_msg"] = vs
				}
				switch matches := reMsg.FindStringSubmatch(value); {
				case len(matches) == 0 || matches[0] == "":
					rowMap["message"] = value
				case matches[1] != "" && matches[2] != "":
					rowMap["duration"], err = strconv.ParseFloat(matches[1], 64)
					if err != nil {
						errChan <- fmt.Errorf("could not read duration: %v", matches[1])
						return
					}
					rowMap["statement"] = matches[2]
				case matches[1] != "":
					rowMap["duration"], err = strconv.ParseFloat(matches[1], 64)
					if err != nil {
						errChan <- fmt.Errorf("could not read duration: %v", matches[1])
						return
					}
					switch rowMap["command_tag"] {
					case "BIND", "PARSE":
						break
					default:
						if statement, ok := statementsCache[rowMap["session_id"]]; ok {
							rowMap["statement"] = statement
							delete(statementsCache, rowMap["session_id"])
						}
					}
				case matches[2] != "":
					statementsCache[rowMap["session_id"]] = matches[2]
				}
			case "error_severity":
				switch value {
				case "ERROR", "FATAL":
					if statement, ok := statementsCache[rowMap["session_id"]]; ok {
						rowMap["statement"] = statement
						delete(statementsCache, rowMap["session_id"])
					}
				default:
					rowMap["error_serverity"] = value
				}
			case "query":
				if depersonalize {
					rowMap["query"] = reDepers.ReplaceAllString(value, "$1 ( DEPERSONALIZED )")
				} else {
					rowMap["query"] = value
				}
			default:
				rowMap[pgCsvLogFields[index]] = value
			}
		}
		gelfChan <- rowMap
	}
}

func graylogWriter(
	graylogAddress string,
	rowMapChan <-chan map[string]interface{},
	errChan chan<- error) {
	hostname, err := os.Hostname()
	if err != nil {
		errChan <- fmt.Errorf("problem getting hostname: %w", err)
		return
	}
	log.Println("Begin expoting logs to graylog server:", graylogAddress)
	gelfWriter, err := gelf.NewUDPWriter(graylogAddress)
	if err != nil {
		errChan <- fmt.Errorf("problem setting up UPDGelf: %w", err)
		return
	}
	defer gelfWriter.Close()

	for rowMap := range rowMapChan {
		message := gelf.Message{
			Version:  "1.1",
			Host:     hostname,
			Short:    "",
			Full:     "",
			TimeUnix: 0.0,
			Level:    1,
			Facility: facility,
			Extra:    nil,
			// RawExtra: json.RawMessage,
		}
		if ts, ok := rowMap["log_time"].(time.Time); ok {
			message.TimeUnix = float64(ts.Unix()) + (float64(ts.Nanosecond()) / nanoSec)
			delete(rowMap, "log_time")
		} else {
			log.Println("timestamp is missing in a row")
			continue
		}
		if msg, ok := rowMap["message"].(string); ok {
			if len(msg) > shortMsgLen {
				message.Short = msg[:shortMsgLen]
				message.Full = msg
			} else {
				message.Short = msg
				message.Full = ""
			}
			delete(rowMap, "message")
		} else if statement, ok := rowMap["statement"].(string); ok {
			if len(statement) > shortMsgLen {
				message.Short = statement[:shortMsgLen]
			} else {
				message.Short = statement
			}
		}
		message.Extra = rowMap
		err = gelfWriter.WriteMessage(&message)
		if err != nil {
			errChan <- fmt.Errorf("error writing message (UDP GELF): %w", err)
			return
		}
	}
}

type logFile struct {
	// Blocking and ahead read caching io.Reader interface implementation
	*os.File
	watcher   *inotify.Watcher
	blockChan chan []byte
	err       error
	tailBuf   []byte
	cacheSize int
}

func (f *logFile) Seek(offset int64, whence int) (ret int64, err error) {
	ret, err = f.File.Seek(offset, whence)
	if err != nil {
		return
	}
	if !(offset == 0 && whence == io.SeekCurrent) || ret != 0 {
		ret, err = f.AlignToRow()
	}
	return
}

func (f *logFile) AlignToRow() (ret int64, err error) {
	rowPrefix := []byte("\"\n")
	var n, ri int
	for {
		buf := make([]byte, bufSize)
		if n, err = f.File.Read(buf); errors.Is(err, io.EOF) {
			time.Sleep(time.Second)
			continue
		} else if err != nil {
			return ret, fmt.Errorf("error align to next row: %w", err)
		}
		for {
			var i int
			if i = bytes.Index(buf, rowPrefix); i == -1 {
				break
			} else if ri = i + len(rowPrefix); n-ri < len(logTimeFormat) {
				_, err = f.File.Seek(int64(i-n), io.SeekCurrent)
				if err != nil {
					return
				}
				break
			} else if _, err := time.Parse(
				logTimeFormat,
				string(buf[ri:ri+len(logTimeFormat)])); err != nil {
				continue
			}
			return f.File.Seek(int64(ri-n), io.SeekCurrent)
		}
	}
}

func (f *logFile) Read(b []byte) (n int, err error) {
	if f.blockChan == nil {
		f.blockChan = make(chan []byte, f.cacheSize)
		go f.readAhead()
	}

	if f.tailBuf == nil || len(f.tailBuf) == 0 {
		var ok bool
		f.tailBuf, ok = <-f.blockChan
		if !ok {
			f.File.Close()
			f.watcher.Close()
			return 0, f.err
		}
	}

	switch n = copy(b, f.tailBuf); {
	case n == len(f.tailBuf):
		f.tailBuf = nil
	case n == len(b):
		f.tailBuf = f.tailBuf[n:]
	}
	return
}

func (f *logFile) readAhead() {
	defer close(f.blockChan)
	if err := f.watcher.AddWatch(f.File.Name(), inotify.InModify|inotify.InCloseWrite); err != nil {
		f.err = err
		return
	}
	defer func() {
		if err := f.watcher.RemoveWatch(f.File.Name()); err != nil {
			f.err = err
			return
		}
	}()
	for {
		select {
		case event := <-f.watcher.Event:
			for {
				bs := make([]byte, bufSize)
				n, err := f.File.Read(bs)
				if errors.Is(err, io.EOF) {
					if event.Mask&inotify.InCloseWrite != 0 {
						f.err = io.EOF
						return
					}
					break
				} else if err != nil {
					f.err = err
					return
				}
				f.blockChan <- bs[0:n:n]
			}
		case <-time.After(time.Second):
			runtime.GC()
		case err := <-f.watcher.Error:
			f.err = err
			return
		}
	}
}

func (f *logFile) Close() {
	f.watcher.Close()
	f.File.Close()
}

func OpenLogFile(name string, cacheSize int) (f *logFile, err error) {
	var file *os.File
	var w *inotify.Watcher
	file, err = os.Open(name)
	if err != nil {
		return nil, err
	}
	w, err = inotify.NewWatcher()
	if err != nil {
		file.Close()
		return nil, err
	}
	lf := &logFile{
		File:      file,
		cacheSize: cacheSize,
		tailBuf:   make([]byte, 0, bufSize),
		watcher:   w,
	}
	return lf, err
}
