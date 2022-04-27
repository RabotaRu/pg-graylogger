package main

import (
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/RabotaRu/pg-graylogger/logfile"
	"gopkg.in/Graylog2/go-gelf.v2/gelf"
	"k8s.io/utils/inotify"
)

const (
	bufSize       = 1024 * 1024
	logTimeFormat = "2006-01-02 15:04:05.000 MST"
	secRndPos     = 16
	shortMsgLen   = 100
)

var (
	Version                                 = "dev"
	graylogAddress, logDir, facility, debug string
	cacheSize                               int
	depersonalize, showVer                  bool
	statementsCache                         sync.Map
	ppwg, gwwg                              sync.WaitGroup
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
		"leader_pid",
		"query_id",
	}
	reMsg = regexp.MustCompile("(?is)" +
		`^(?:duration:\s(?P<duration>\d+\.\d{3})\sms\s*|)` +
		`(?:(?:statement|execute .+?):\s*(?P<statement>.*?)\s*|)$`)
	reValues = regexp.MustCompile(`(?si)\s+(VALUES|IN|\)\s*=)\s*\(`)
	reSubReq = regexp.MustCompile(`(?si)^\s*(INSERT|SELECT|UPDATE)\s+`)
)

func CleanQuery(q *string) {
	query := (*q)
	var ppos int
	var depsql string
	for {
		var pos int
		if i := reValues.FindStringIndex(query[ppos:]); i != nil {
			pos = ppos + i[1]
		} else {
			break
		}
		if i := reSubReq.FindStringIndex(query[pos:]); i != nil {
			ppos = pos + i[1]
			continue
		}
		var inum, closed_brackets, temp_pos int
		var subtext bool
		opened_brackets := 1
		depsql += query[ppos:pos]
	findBlock:
		for i, c := range query[pos:] {
			if subtext && c != '\'' {
				continue
			}
			switch c {
			case '\'':
				subtext = !subtext
			case ',':
				inum++
			case ' ', '\t', '\f', '\n', '\r':
			case '(':
				opened_brackets++
			case ')':
				if closed_brackets == opened_brackets {
					break findBlock
				}
				closed_brackets++
				if closed_brackets == opened_brackets {
					inum++
					temp_pos = pos + i
				}
			default:
				if closed_brackets == opened_brackets {
					break findBlock
				}
			}
		}
		if temp_pos != 0 {
			if closed_brackets > 0 {
				depsql += fmt.Sprintf("/* HIDDEN %v VALUES %v symbols */", closed_brackets, temp_pos-ppos)
			} else {
				depsql += fmt.Sprintf("/* HIDDEN %v ITEMS %v symbols */", inum, temp_pos-ppos)
			}
			ppos = temp_pos
		}
	}
	depsql += query[ppos:]
	*q = depsql
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			log.Fatalln(r)
		}
	}()

	flag.StringVar(&graylogAddress, "graylog-address", "localhost:2345",
		"Address of graylog in form of server:port")
	flag.StringVar(&logDir, "log-dir", "/var/log/postgresql",
		"Path to postgresql log file in csv format")
	compresionLevel := flag.Int("compression-level", 5,
		"Compression level for gelf packets")
	compType := flag.String("compressioin-type", "gzip",
		"Compression type (gzip, zlib or none)")
	flag.IntVar(&cacheSize, "cache-size", 10, "ReadAhead buffer cache size")
	procThreads := flag.Int("processing-threads", 1, "Number of record-processing threads")
	gelfStreams := flag.Int("gelf-streams", 1, "Number of UDP GELF forming streams")
	flag.StringVar(&facility, "facility", "", "Facility field for log messages")
	flag.BoolVar(&depersonalize, "depers", false,
		"Depersonalize. Replace sensible information (field values) from query texts")
	flag.BoolVar(&showVer, "version", false, "Show version")

	flag.Parse()

	if showVer {
		fmt.Println(Version)
		return
	}

	if *procThreads <= 0 {
		panic("Number of processing worker threads must be positive!")
	}

	if *gelfStreams <= 0 {
		panic("Number of GELF streams must be positive!")
	}

	if debug = os.Getenv("DEBUG"); debug != "" {
		go func() { log.Println(http.ListenAndServe(debug, nil)) }()
	}

	gelfWriter, err := gelf.NewUDPWriter(graylogAddress)
	if err != nil {
		panic(fmt.Errorf("problem setting up UDPGelf: %w", err))
	}
	defer gelfWriter.Close()

	switch *compType {
	case "gzip":
		gelfWriter.CompressionType = gelf.CompressGzip
	case "zlib":
		gelfWriter.CompressionType = gelf.CompressZlib
	case "none":
		gelfWriter.CompressionType = gelf.CompressNone
	default:
		panic(fmt.Sprintf("%v is not a valid value for -compression-type", *compType))
	}
	gelfWriter.CompressionLevel = *compresionLevel
	log.Println("Ready for expoting logs to graylog server:", graylogAddress)

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	preprocChan := make(chan []string, *procThreads)
	graylogChan := make(chan map[string]interface{}, *gelfStreams)
	errChan := make(chan error)

	for i := 0; i <= *gelfStreams; i++ {
		gwwg.Add(1)
		go graylogWriter(gelfWriter, graylogChan, errChan)
	}
	for i := 0; i <= *procThreads; i++ {
		ppwg.Add(1)
		go rowsPreproc(preprocChan, graylogChan, errChan)
	}
	go csvLogReader(logDir, preprocChan, errChan, signalChan)

	go func() {
		ppwg.Wait()
		close(graylogChan)
		gwwg.Wait()
		close(errChan)
	}()

	err, ok := <-errChan
	if ok && err != nil {
		panic(err)
	} else {
		fmt.Println("")
	}
}

// csvLogReader reads postgres log files in csv format.
// It reads log until ModifyClose event, ignoring intermediate io.EOFs.
// First opened log it reads from end of file, while others reads from beginning.
func csvLogReader(logDir string, rowChan chan<- []string,
	errChan chan<- error, signalChan <-chan os.Signal) {
	defer close(rowChan)
	var log_file *logfile.LogFile
	defer func() {
		if log_file != nil {
			log_file.Close()
		}
	}()
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

		for skip := 0; skip < 2; {
			select {
			case event = <-watcher.Event:
				if !strings.HasSuffix(event.Name, ".csv") {
					continue
				}
				// Workaround for one weird event after log file InCloseWrite
				if log_file == nil || event.Name != log_file.Name() {
					skip++
				}
				skip++
			case err = <-watcher.Error:
				errChan <- fmt.Errorf("inotify watch error: %w", err)
				return
			case <-signalChan:
				return
			}
		}
		log_file, err = logfile.OpenLogFile(event.Name, logTimeFormat, secRndPos, bufSize, cacheSize)
		if err != nil {
			errChan <- fmt.Errorf("error open log file: %w", err)
			return
		}
		// When log_file has just been creanted and before first csvReader init
		// we move to end of first log file. But we dont want to scroll any of next log files.
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

		for reading := true; reading; {
			select {
			case <-signalChan:
				return
			default:
				row, err := csvReader.Read()
				if errors.Is(err, io.EOF) {
					log_file.Close()
					runtime.GC()
					reading = false
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
}

// rowsPreproc procceses raw slices of strings from csvLogReader and sends them to graylogWriter
func rowsPreproc(rowChan <-chan []string,
	gelfChan chan<- map[string]interface{},
	errChan chan<- error) {
	defer ppwg.Done()

	var err error

	for row := range rowChan {
		rowMap := make(map[string]interface{}, 32)
		for index, value := range row {
			switch pgCsvLogFields[index] {
			case "message":
				if depersonalize {
					CleanQuery(&value)
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
						rowMap["message"] = rowMap["command_tag"]
					default:
						if statement, loaded := statementsCache.LoadAndDelete(rowMap["session_id"]); loaded {
							rowMap["statement"] = statement
						}
					}
				case matches[2] != "":
					statementsCache.Store(rowMap["session_id"], matches[2])
				}
			case "error_severity":
				switch value {
				case "ERROR", "FATAL":
					if statement, loaded := statementsCache.LoadAndDelete(rowMap["session_id"]); loaded {
						rowMap["statement"] = statement
					}
				default:
					rowMap["error_serverity"] = value
				}
			case "query":
				if depersonalize {
					CleanQuery(&value)
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

// graylogWriter sends preproccesed log messages to graylog server with UDPGelf
func graylogWriter(
	gelfWriter *gelf.UDPWriter,
	rowMapChan <-chan map[string]interface{},
	errChan chan<- error) {
	defer gwwg.Done()

	hostname, err := os.Hostname()
	if err != nil {
		errChan <- fmt.Errorf("problem getting hostname: %w", err)
		return
	}

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

		var msg string
		var ok bool
		if msg, ok = rowMap["statement"].(string); ok {
		} else if msg, ok = rowMap["message"].(string); ok {
			delete(rowMap, "message")
		} else {
			msg = "empty"
		}
		if len(msg) > shortMsgLen {
			message.Short = msg[:shortMsgLen]
			if _, ok = rowMap["statement"]; !ok {
				message.Full = msg
			}
		} else {
			message.Short = msg
		}
		message.Extra = rowMap

		err = gelfWriter.WriteMessage(&message)
		if err != nil {
			switch err_msg := err.Error(); {
			case strings.HasPrefix(err_msg, "msg too large"):
				var ss, qs int
				if q, ok := rowMap["query"].(string); ok {
					qs = len(q)
				}
				if s, ok := rowMap["statement"].(string); ok {
					ss = len(s)
				}
				log.Printf(
					"SKIPPED, could't send message from %v with session_id %v and session_linenum %v: %v, %v %v \n",
					rowMap["log_time"], rowMap["session_id"], rowMap["session_line_num"], err_msg, ss, qs)
			default:
				errChan <- fmt.Errorf("error writing message (UDP GELF): %w", err)
				return
			}
		}
	}
}
