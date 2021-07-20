package main

import (
	"bytes"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	// csv "github.com/JensRantil/go-csv" // Alternative pythonistic but revers-compatible implementaion
	"gopkg.in/Graylog2/go-gelf.v2/gelf"
	"k8s.io/utils/inotify"
)

const (
	Version       = "v0.1.0"
	bufSize       = 1048576
	logTimeFormat = "2006-01-02 15:04:05.000 MST"
	shortMsgLen   = 100
	nanoSec       = 1000000000.0
)

var (
	Debug, showVer, depersonalize bool
	logDir                        string
	graylogAddress                string
	pgCsvLogFields                = [...]string{
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
	rowcache  = make(map[string]string, 1024)
	re_msg    = regexp.MustCompile(`(?is)^(?:duration:\s(?P<duration>\d+\.\d{3})\sms\s*|)(?:(?:statement|execute .+?):\s*(?P<statement>.*?)\s*|)$`)
	re_depers = regexp.MustCompile(`(?is)(VALUES|IN)\s*\((.*?)\)`)
)

func main() {
	if showVer {
		fmt.Println(Version)
		os.Exit(0)
	}
	watcher, err := inotify.NewWatcher()
	if err != nil {
		log.Fatalln("Error setting up inotify watcher", err)
	}
	//err = watcher.AddWatch(logDir, inotify.InModify) // nolint:typecheck
	//if err != nil {
	//	log.Fatalln("Error add inotify watch", err)
	//}

	preprocChan := make(chan []string)
	graylogChan := make(chan map[string]interface{})

	go graylogWriter(graylogAddress, depersonalize, graylogChan)
	go rowsPreproc(preprocChan, graylogChan)
	csvLogReader(watcher, preprocChan)
}

func init() {
	flag.StringVar(&graylogAddress, "graylog-address", "localhost:2345",
		"Address of graylog in form of server:port")
	flag.StringVar(&logDir, "log-dir", "/var/log/postgresql",
		"Path to postgresql log file in csv format")
	flag.BoolVar(&depersonalize, "depers", false,
		"Depersonalize. Replace sensible information (field values) from query texts")
	flag.BoolVar(&showVer, "version", false, "Show version")
	flag.Parse()
	_, Debug = os.LookupEnv("DEBUG")
}

func csvLogReader(watcher *inotify.Watcher, rowChan chan<- []string) {
	// First event used for setting up reader and not checkin it lately in every iteration
	var err error
	var event *inotify.Event
	var logFile *os.File
	var reader *csv.Reader
	for event = range watcher.Event {
		if strings.HasSuffix(event.Name, ".csv") {
			logFile, err = os.Open(event.Name)
			if err != nil {
				log.Fatalln("Error open log file", err)
			}
			reader = csv.NewReader(logFile)
			reader.FieldsPerRecord = 0
			reader.Read() //Read first row for setting number of field
			logFile.Seek(0, os.SEEK_END)
			break
		}
	}
	for {
		select {
		case event = <-watcher.Event:
			if !strings.HasSuffix(event.Name, ".csv") {
				continue
			}
		case err := <-watcher.Error:
			log.Fatalln("Inotify watch error:", err)
		}
		if logFile.Name() != event.Name {
			logFile.Close()
			logFile, err = os.Open(event.Name)
			if err != nil {
				log.Fatalln("Error open next log file:", err)
			}
			fmt.Println("Begin reading new log file", event.Name, "at", time.Now())
			reader = csv.NewReader(logFile)
			reader.FieldsPerRecord = 0
		}
		for {
			row := make([]string, 0, len(pgCsvLogFields)+1)
			row, err = reader.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				err = AlignToRow(logFile)
				if err != nil {
					log.Fatalln("Error reading next row", err)
				}
				continue
			}
			rowChan <- row
		}
	}
	close(rowChan)
}

func rowsPreproc(rowChan <-chan []string, gelfChan chan<- map[string]interface{}) (err error) {
	defer close(gelfChan)
	for row := range rowChan {
		rowmap := make(map[string]interface{}, 32)
		for index, value := range row {
			switch pgCsvLogFields[index] {
			case "log_time":
				{
					rowmap["log_time"], err = time.Parse(logTimeFormat, value)
					if err != nil {
						log.Fatalln("Coundn't parse log time.", err)
					}
				}
			case "message":
				{
					switch matches := re_msg.FindStringSubmatch(value); {
					case matches == nil || matches[0] == "":
						{
							rowmap["message"] = value
						}
					case matches[1] != "" && matches[2] != "":
						{
							rowmap["duration"], err = strconv.ParseFloat(matches[1], 64)
							if err != nil {
								log.Fatalln("Could not read duration:", matches[1])
							}
							if depersonalize {
								rowmap["statement"] = re_depers.ReplaceAllString(matches[2],
									"$1 ( DEPERSONALIZED )")
							} else {
								rowmap["statement"] = matches[2]
							}
						}
					case matches[1] != "":
						{
							var ok bool
							rowmap["duration"], err = strconv.ParseFloat(matches[1], 64)
							if err != nil {
								log.Fatalln("Could not read duration:", matches[1])
							}
							rowmap["statement"], ok = rowcache[rowmap["session_id"].(string)]
							if ok {
								delete(rowcache, rowmap["session_id"].(string))
							}
						}
					case matches[2] != "":
						{
							if depersonalize {
								rowcache[rowmap["session_id"].(string)] = re_depers.ReplaceAllString(
									matches[2],
									"$1 ( DEPERSONALIZED )")
							} else {
								rowcache[rowmap["session_id"].(string)] = matches[2]
							}
						}
					}
				}
			default:
				{
					rowmap[pgCsvLogFields[index]] = value
				}
			}
		}
		gelfChan <- rowmap
	}
	return err
}

func graylogWriter(graylogAddress string, depersonalize bool,
	rowMapChan <-chan map[string]interface{}) (err error) {
	fmt.Println("Begin logging to graylog server:", graylogAddress)
	gelfWriter, err := gelf.NewUDPWriter(graylogAddress)
	if err != nil {
		log.Fatalln("Error setting up UPDGelf:", err)
	}
	defer gelfWriter.Close()

	hostname, err := os.Hostname()
	if err != nil {
		log.Fatalln("Error to get hostname.", err)
	}
	message := gelf.Message{
		Version:  "1.1",
		Host:     hostname,
		Short:    "",
		Full:     "",
		TimeUnix: 0.0,
		Level:    1,
		Facility: "",
		Extra:    nil,
		// RawExtra: json.RawMessage,
	}

	for rowmap := range rowMapChan {
		ts := rowmap["log_time"].(time.Time)
		message.TimeUnix = float64(ts.Unix()) + (float64(ts.Nanosecond()) / nanoSec)
		delete(rowmap, "log_time")
		if msg, ok := rowmap["message"]; ok {
			if len(msg.(string)) > shortMsgLen {
				message.Short = msg.(string)[:shortMsgLen]
				message.Full = msg.(string)
			} else {
				message.Short = msg.(string)
				message.Full = ""
			}
			delete(rowmap, "message")
		} else {
			message.Full = ""
			if _, ok := rowmap["statement"]; ok {
				if len(rowmap["statement"].(string)) > shortMsgLen {
					message.Short = rowmap["statement"].(string)[:shortMsgLen]
				} else {
					message.Short = rowmap["statement"].(string)
				}
			}
		}
		message.Extra = rowmap
		gelfWriter.WriteMessage(&message)
	}
	return err
}

func AlignToRow(file *os.File) (err error) {
	curTimeStr := "\n" + time.Now().Format(logTimeFormat[:16])
	// buf := make([]byte, 0, bufSize)
	var bufArr [bufSize]byte
	buf := bufArr[:]
	index := -1
	var n int
	pos, err := file.Seek(0, os.SEEK_CUR)
	for i := 1; index == -1; i++ {
		pos, err = file.Seek(pos-bufSize, os.SEEK_SET)
		if err != nil {
			log.Fatalln("Could not seek to", i, "Mb from end of log file", err)
		}
		n, err = file.Read(buf)
		if err != nil {
			return err
		}
		index = bytes.LastIndex(buf, []byte(curTimeStr))
	}
	file.Seek(int64(index-n+1), os.SEEK_CUR)
	return err
}
