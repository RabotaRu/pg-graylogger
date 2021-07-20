package main
import (
	"fmt"
	"flag"
	"os"
	"io"
	"log"
	"strings"
	"strconv"
	"bytes"
	"time"
	"regexp"
	"encoding/csv"
// csv "github.com/JensRantil/go-csv" // Alternative pythonistic but revers-compatible implementaion
	"k8s.io/utils/inotify"
	"gopkg.in/Graylog2/go-gelf.v2/gelf"
)
const (
	VERSION = "0.1"
	buf_size = 1048576
	log_time_format = "2006-01-02 15:04:05.000 MST"
	short_msg_len = 100
)

var (
	DEBUG bool
	show_ver bool
	log_dir string
	graylog_address string
	depersonalize bool
	pgcsvlog_fields = [...]string{
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
	rowcache = make(map[string]string, 1024)
	re_msg = regexp.MustCompile(`(?is)^(?:duration:\s(?P<duration>\d+\.\d{3})\sms\s*|)(?:(?:statement|execute .+?):\s*(?P<statement>.*?)\s*|)$`);
	re_depers = regexp.MustCompile(`(?is)(VALUES|IN)\s*\((.*?)\)`)
)

func main() {
	cli()
	watcher, err := inotify.NewWatcher()
	if err != nil { log.Fatalln("Error setting up inotify watcher", err) }
	err = watcher.AddWatch(log_dir, inotify.InModify)
	if err != nil { log.Fatalln("Error add inotify watch", err) }

	preproc_channel := make(chan []string)
	graylog_channel := make(chan map[string]interface{})

	go GraylogWriter(graylog_address, depersonalize, graylog_channel)
	go RowsPreprocessor(preproc_channel, graylog_channel)
	CsvlogReader(watcher, preproc_channel)
}

func cli(){
	flag.StringVar(&graylog_address, "graylog-address", "localhost:2345",
		"Address of graylog in form of server:port")
	flag.StringVar(&log_dir, "log-dir", "/var/log/postgresql",
		"Path to postgresql log file in csv format")
	flag.BoolVar(&depersonalize, "depers", false,
		"Depersonalize. Replace sensible information (field values) from query texts")
	flag.BoolVar(&show_ver, "version", false, "Show version")
	flag.Parse()
	_, DEBUG = os.LookupEnv("DEBUG");
	if show_ver{
		fmt.Println(VERSION)
	}
}

func CsvlogReader(watcher *inotify.Watcher, row_ch chan <- []string){
	// First event used for setting up reader and not checkin it lately in every iteration
	var err error
	var event *inotify.Event
	var logFile *os.File
	var reader *csv.Reader
	for event = range watcher.Event{
		if strings.HasSuffix(event.Name, ".csv") {
			logFile, err = os.Open(event.Name)
			if err != nil { log.Fatalln("Error open log file", err) }
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
			if err != nil { log.Fatalln("Error open next log file:", err) }
			fmt.Println("Begin reading new log file", event.Name, "at", time.Now())
			reader = csv.NewReader(logFile)
			reader.FieldsPerRecord = 0
		}
		for {
			row := make([]string, 0, len(pgcsvlog_fields)+1);
			row, err = reader.Read()
			if err == io.EOF {
				break
			}
			if err != nil{
				err = AlignToRow(logFile)
				if err != nil { log.Fatalln("Error reading next row", err) }
				continue
			}
			row_ch <- row
		}
	}
	close(row_ch)
}

func RowsPreprocessor(row_ch <- chan []string, gelf_ch chan <- map[string]interface{}){
	var err error
	for row := range row_ch {
		rowmap := make(map[string]interface{}, 32)
		for i, v := range row{
			switch pgcsvlog_fields[i] {
			case "log_time": {
				rowmap["log_time"], err = time.Parse(log_time_format, v)
				if err != nil { log.Fatalln("Coundn't parse log time.", err) }
				}
			case "message": {
				switch matches := re_msg.FindStringSubmatch(v); {
				case matches == nil || matches[0] == "": {
					rowmap["message"] = v
					}
				case matches[1] != "" && matches[2] != "": {
					rowmap["duration"], err = strconv.ParseFloat(matches[1], 64)
					if err != nil { log.Fatalln("Could not read duration:", matches[1]) }
					if depersonalize {
						rowmap["statement"] = re_depers.ReplaceAllString(matches[2],
							"$1 ( DEPERSONALIZED )")
					} else {
						rowmap["statement"] = matches[2]
					}
				}
				case matches[1] != "": {
					var ok bool
					rowmap["duration"], err = strconv.ParseFloat(matches[1], 64)
					if err != nil { log.Fatalln("Could not read duration:", matches[1]) }
					rowmap["statement"], ok = rowcache[rowmap["session_id"].(string)]
					if ok {
						delete(rowcache, rowmap["session_id"].(string))
					}
				}
				case matches[2] != "": {
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
			default: {
						rowmap[pgcsvlog_fields[i]] = v
			}
			}
		}
		gelf_ch <- rowmap
	}
	close(gelf_ch)
}

func GraylogWriter(graylog_address string, depersonalize bool,
					rowmap_ch <- chan map[string]interface{}) {
	fmt.Println("Begin logging to graylog server:", graylog_address)
	gelfWriter, err := gelf.NewUDPWriter(graylog_address)
	if err != nil { log.Fatalln("Error setting up UPDGelf:", err) }
	defer gelfWriter.Close()

	hostname, err := os.Hostname() ; if err != nil { log.Fatalln("Error to get hostname.", err) }
	message := gelf.Message{
		Version: "1.1",
		Host: hostname,
		Short: "",
		Full: "",
		TimeUnix: 0.0,
		Level: 1,
		Facility: "",
		Extra: nil,
		//~ RawExtra: json.RawMessage,
		}

	for rowmap := range rowmap_ch{
		ts := rowmap["log_time"].(time.Time)
		message.TimeUnix = float64(ts.Unix()) + (float64(ts.Nanosecond()) / 1000000000.)
		delete(rowmap, "log_time")
		if msg, ok := rowmap["message"]; ok {
			if len(msg.(string)) > short_msg_len {
				message.Short = msg.(string)[:short_msg_len]
				message.Full = msg.(string)
			} else {
				message.Short = msg.(string)
				message.Full = ""
			}
			delete(rowmap, "message")
		} else {
			message.Full = ""
			if _, ok := rowmap["statement"]; ok {
				if len(rowmap["statement"].(string)) > short_msg_len {
					message.Short = rowmap["statement"].(string)[:short_msg_len]
				} else {
					message.Short = rowmap["statement"].(string)
				}
			}
		}
		message.Extra = rowmap
		gelfWriter.WriteMessage(&message)
	}
}

func AlignToRow(file *os.File)(err error){
	cur_time_str := "\n" + time.Now().Format(log_time_format[:16])
	// buf := make([]byte, 0, buf_size)
	var bufarr [buf_size]byte
	buf := bufarr[:]
	ind := -1
	var i,n int
	pos, err := file.Seek(0, os.SEEK_CUR )
	for i = 1 ; ind == -1 ; i++ {
		pos, err = file.Seek(pos - buf_size, os.SEEK_SET )
		if err != nil { log.Fatalln("Could not seek to", i, "Mb from end of log file", err) }
		n, err = file.Read(buf)
		if err != nil { return err }
		ind = bytes.LastIndex(buf, []byte(cur_time_str))
	}
	file.Seek(int64(ind-n+1), os.SEEK_CUR)
	return err
}
