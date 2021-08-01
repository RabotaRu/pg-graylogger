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
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/Graylog2/go-gelf.v2/gelf"
)

const (
	Version       = "v0.1.0"
	bufSize       = 1024 * 1024
	logTimeFormat = "2006-01-02 15:04:05.000 MST"
	shortMsgLen   = 100
	nanoSec       = 1000000000.0
)

var (
	Debug, showVer, depersonalize    bool
	logDir, graylogAddress, facility string
	pgCsvLogFields                   = [...]string{
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
	statementsCache = make(map[string]string, 1024)
	reMsg           = regexp.MustCompile("(?is)" +
		`^(?:duration:\s(?P<duration>\d+\.\d{3})\sms\s*|)` +
		`(?:(?:statement|execute .+?):\s*(?P<statement>.*?)\s*|)$`)
	reDepers = regexp.MustCompile(`(?is)(VALUES|IN)\s*\((.*?)\)`)
)

func main() {
	cli()

	if Debug {
		go func() { log.Println(http.ListenAndServe("localhost:6060", nil)) }()
	}
	preprocChan := make(chan []string)
	graylogChan := make(chan map[string]interface{})
	errChan := make(chan error)

	go graylogWriter(graylogAddress, graylogChan, errChan)
	go rowsPreproc(preprocChan, graylogChan, errChan)
	go csvLogReader(logDir, preprocChan, errChan)
	// if err != nil {
	// log.Fatalln("CSV log reader returned with error", err)
	// }
	var ok bool
	err, ok := <-errChan
	if ok && err != nil {
		log.Fatalln(err)
	}
}

func cli() {
	flag.StringVar(&graylogAddress, "graylog-address", "localhost:2345",
		"Address of graylog in form of server:port")
	flag.StringVar(&logDir, "log-dir", "/var/log/postgresql",
		"Path to postgresql log file in csv format")
	flag.StringVar(&facility, "facility", "", "Facility field for log messages")
	flag.BoolVar(&depersonalize, "depers", false,
		"Depersonalize. Replace sensible information (field values) from query texts")
	flag.BoolVar(&showVer, "version", false, "Show version")
	flag.Parse()

	_, Debug = os.LookupEnv("DEBUG")

	if showVer {
		fmt.Println(Version)
		os.Exit(0)
	}
}

func csvLogReader(logDir string, rowChan chan<- []string, errChan chan<- error) {
	// First event used for setting up reader and not checkin it lately in every iteration
	defer close(rowChan)
	var event fsnotify.Event
	var logFile *os.File
	var reader *csv.Reader
	var row []string
	var watcher *fsnotify.Watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		errChan <- err
		return
	}
	defer watcher.Close()
	err = watcher.Add(logDir)
	if err != nil {
		errChan <- err
		return
	}

	for event = range watcher.Events {
		if event.Op != fsnotify.Write || !strings.HasSuffix(event.Name, ".csv") {
			continue
		}
		log.Println("Begin reading log file", event.Name)
		logFile, err = os.Open(event.Name)
		if err != nil {
			errChan <- err
			return
			// log.Fatalln("Error open log file", err)
		}
		reader = csv.NewReader(logFile)
		reader.FieldsPerRecord = 0
		_, err = reader.Read() // Read first row for setting number of fields
		if err != nil {
			errChan <- err
			return
		}
		_, err = logFile.Seek(0, io.SeekEnd)
		if err != nil {
			errChan <- err
			return
		}
		break
	}
	for {
		select {
		case event := <-watcher.Events:
			if event.Op != fsnotify.Write && !strings.HasSuffix(event.Name, ".csv") {
				continue
			}
		case err = <-watcher.Errors:
			errChan <- err
			return
			// log.Fatalln("Fsnotify watch error:", err)
		}
		if logFile.Name() != event.Name {
			logFile.Close()
			logFile, err = os.Open(event.Name)
			if err != nil {
				errChan <- err
				return
				// log.Fatalln("Error open next log file:", err)
			}
			log.Println("Begin reading new log file", event.Name, "at", time.Now())
			reader = csv.NewReader(logFile)
			reader.FieldsPerRecord = 0
		}
		err = watcher.Remove(logDir)
		if err != nil {
			errChan <- err
			return
		}
		for {
			// row := make([]string, 0, len(pgCsvLogFields)+5)
			row, err = reader.Read()
			if errors.Is(err, io.EOF) {
				if Debug {
					log.Println("STATEMENT CACHE SIZE:", len(statementsCache))
				}
				time.Sleep(time.Second)
				break
			}
			if err != nil {
				err = AlignToRow(logFile)
				if err != nil {
					errChan <- err
					return
					// log.Fatalln("Error reading next row", err)
				}
				reader = csv.NewReader(logFile)
				time.Sleep(time.Second)
				continue
			}
			rowChan <- row
		}
		err = watcher.Add(logDir)
		if err != nil {
			errChan <- err
			return
		}
	}
}

func rowsPreproc(rowChan <-chan []string,
	gelfChan chan<- map[string]interface{},
	errChan chan<- error) {
	var err error
	defer close(gelfChan)
	for row := range rowChan {
		rowMap := make(map[string]interface{}, 32)
		for index, value := range row {
			switch pgCsvLogFields[index] {
			case "log_time":
				rowMap["log_time"], err = time.Parse(logTimeFormat, value)
				if err != nil {
					errChan <- err
					return
					// log.Fatalln("Coundn't parse log time.", err)
				}
			case "message":
				switch matches := reMsg.FindStringSubmatch(value); {
				case len(matches) == 0 || matches[0] == "":
					rowMap["message"] = value
				case matches[1] != "" && matches[2] != "":
					rowMap["duration"], err = strconv.ParseFloat(matches[1], 64)
					if err != nil {
						errChan <- err
						return
						// log.Fatalln("Could not read duration:", matches[1])
					}
					if depersonalize {
						rowMap["statement"] = reDepers.ReplaceAllString(matches[2],
							"$1 ( DEPERSONALIZED )")
					} else {
						rowMap["statement"] = matches[2]
					}
				case matches[1] != "":
					var ok bool
					rowMap["duration"], err = strconv.ParseFloat(matches[1], 64)
					if err != nil {
						errChan <- err
						return
						// log.Fatalln("Could not read duration:", matches[1])
					}
					rowMap["statement"], ok = statementsCache[rowMap["session_id"].(string)]
					if ok {
						delete(statementsCache, rowMap["session_id"].(string))
					}
				case matches[2] != "":
					if depersonalize {
						statementsCache[rowMap["session_id"].(string)] = reDepers.ReplaceAllString(
							matches[2],
							"$1 ( DEPERSONALIZED )")
					} else {
						statementsCache[rowMap["session_id"].(string)] = matches[2]
					}
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
	var err error
	var hostname string
	hostname, err = os.Hostname()
	if err != nil {
		errChan <- err
		return
		// log.Fatalln("Error to get hostname.", err)
	}
	log.Println("Begin expoting logs to graylog server:", graylogAddress)
	gelfWriter, err := gelf.NewUDPWriter(graylogAddress)
	if err != nil {
		errChan <- err
		return
		// log.Fatalln("Error setting up UPDGelf:", err)
	}
	defer gelfWriter.Close()

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

	for rowMap := range rowMapChan {
		message.Full = ""
		if ts, ok := rowMap["log_time"].(time.Time); ok {
			message.TimeUnix = float64(ts.Unix()) + (float64(ts.Nanosecond()) / nanoSec)
			delete(rowMap, "log_time")
		} else {
			log.Println("No timestamp")
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
			errChan <- err
			return
		}
	}
}

type AlignError struct {
	Err      error
	Position int
}

func (e *AlignError) Error() string {
	return fmt.Sprintf("Could not seek to %vMb from end of log file %v", e.Position, e.Err)
}

func AlignToRow(file *os.File) (err error) {
	curTimeStr := "\n" + time.Now().Format(logTimeFormat[:16])
	// buf := make([]byte, 0, bufSize)
	var bufArr [bufSize]byte
	buf := bufArr[:]
	index := -1
	var n int
	var pos int64
	pos, err = file.Seek(0, io.SeekCurrent)
	if err != nil {
		return
	}
	for i := 1; index == -1; i++ {
		pos, err = file.Seek(pos-bufSize, io.SeekStart)
		if err != nil {
			return &AlignError{Position: i, Err: err}
		}
		n, err = file.Read(buf)
		if err != nil {
			return
		}
		index = bytes.LastIndex(buf[:n], []byte(curTimeStr))
	}
	_, err = file.Seek(int64(index-n+1), io.SeekCurrent)
	return
}
