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
//~ csv "github.com/JensRantil/go-csv" // Alternative pythonistic but revers-compatible implementaion
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
	show_ver bool
	log_dir string
	graylog_address string
	facility string
	depersonalize bool
	pgcsvlog_fields = [...]string{
		"_log_time",
		"_user_name",
		"_database_name",
		"_process_id",
		"_connection_from",
		"_session_id",
		"_session_line_num",
		"_command_tag",
		"_session_start_time",
		"_virtual_transaction_id",
		"_transaction_id",
		"_error_severity",
		"_sql_state_code",
		"_message",
		"_detail",
		"_hint",
		"_internal_query",
		"_internal_query_pos",
		"_context",
		"_query",
		"_query_pos",
		"_location",
		"_application_name",
		"_backend_type",
	}
)

func main() {
	cli()
	//log.SetOutput(io.MultiWriter(os.Stderr, gelfWriter))
	watcher, err := inotify.NewWatcher()
	if err != nil { log.Fatalln("Error setting up inotify watcher", err) }
	err = watcher.AddWatch(log_dir, inotify.InModify)
	if err != nil { log.Fatalln("Error add inotify watch", err) }

	event_channel := make(chan *inotify.Event)
	preproc_channel := make(chan []string)
	graylog_channel := make(chan map[string]interface{})

	go GraylogWriter(graylog_address, depersonalize, graylog_channel)
	go RowsPreprocessor(preproc_channel, graylog_channel)
	go CsvlogReader(event_channel, preproc_channel)

	for {
		select {
		case event := <-watcher.Event:
			if strings.HasSuffix(event.Name, ".csv") {
				event_channel <- event
			}
		case err := <-watcher.Error:
			log.Println("Inotify watch error:", err)
			}
	}
}

func cli(){
	flag.StringVar(&graylog_address, "graylog-address", "localhost:2345",
		"Address of graylog in form of server:port")
	flag.StringVar(&log_dir, "log-dir", "/var/log/postgresql",
		"Path to postgresql log file in csv format")
	flag.StringVar(&facility, "facility", os.Args[0],
		"Facility field for log messages")
	flag.BoolVar(&depersonalize, "depers", false,
		"Depersonalize. Replace sensible information (field values) from query texts")
	flag.BoolVar(&show_ver, "version", false, "Show version")

	flag.Parse()
	if show_ver{
		fmt.Println(VERSION)
	}
}

func CsvlogReader(event_ch <- chan *inotify.Event, row_ch chan <- []string){
	// Firs event used for setting up reader and not checkin it lately in every iteration
	event := <- event_ch
	logFile, err := os.Open(event.Name); if err != nil { log.Fatal(err) }
	reader := csv.NewReader(logFile)
	reader.FieldsPerRecord = 0
	reader.Read() //Read first row for setting number of field
	logFile.Seek(0, os.SEEK_END)
	fmt.Println("Begin reading file", event.Name, "at", time.Now())

	for event = range event_ch{
		for {
			row, err := reader.Read()
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
		if logFile.Name() != event.Name {
			logFile.Close()
			logFile, err := os.Open(event.Name)
			if err != nil { log.Fatalln("Error open next log file:", err) }
			fmt.Println("Begin reading new log file", event.Name, "at", time.Now())
			reader = csv.NewReader(logFile)
			reader.FieldsPerRecord = 0
		}
	}
	close(row_ch)
}

func RowsPreprocessor(row_ch <- chan []string, gelf_ch chan <- map[string]interface{}){
	var err error
	rowcache := map[string]map[string]interface{}{}
	re := regexp.MustCompile(`(?is)(VALUES|IN)\s*\((.*?)\)`)

	for row := range row_ch {
		rowmap := map[string]interface{}{}
		for i, v := range row{
			if i==0{
				ts, err := time.Parse(log_time_format, v)
				if err != nil { log.Fatalln("Coundn't parse log time.", err) }
				rowmap[pgcsvlog_fields[i]] = ts
			} else {
				rowmap[pgcsvlog_fields[i]] = v
			}
		}
		if rowmap["_comand_tag"] != "" {
			if strings.HasPrefix(rowmap["_message"].(string), "statement: "){
				if depersonalize {
					rowmap["_message"] = re.ReplaceAllString(rowmap["_message"].(string),
						"$1 ( DEPERSONALIZED )")
				}
				rowcache[rowmap["_session_id"].(string)] = rowmap
				continue
			} else if strings.HasPrefix(rowmap["_message"].(string), "duration: "){
				row, ok := rowcache[rowmap["_session_id"].(string)]
				if ok {
					delete(rowcache, rowmap["_session_id"].(string))
					row["_duration"], err = strconv.ParseFloat(
						rowmap["_message"].(string)[
							len("duration: "):len(rowmap["_message"].(string))-3],
						64)
					if err != nil { log.Fatalln("Couldn't get statement duration as float", err) }
					gelf_ch <- row
					continue
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
		ts := rowmap["_log_time"].(time.Time)
		message.TimeUnix = float64(ts.Unix()) + (float64(ts.Nanosecond()) / 1000000000.)
		delete(rowmap, "_log_time")
		msg := rowmap["_message"].(string)
		if len(msg) > short_msg_len {
			ind := strings.Index(msg, "\n")
			if ind >= 0{
				message.Short = msg[:ind]
			}else{
				message.Short = msg[:short_msg_len+1]
			}
			message.Full = msg
		} else {
			message.Short = msg
			message.Full = ""
		}

		delete(rowmap, "_message")
		message.Extra = rowmap
		gelfWriter.WriteMessage(&message)
	}
}

func AlignToRow(file *os.File)(err error){
	cur_time_str := time.Now().Format(log_time_format[:17])
	var bufarr [buf_size]byte
	buf := bufarr[:]
	ind := -1
	var i,n int
	for i = 1 ; ind == -1 ; i++ {
		_,err := file.Seek(-buf_size, os.SEEK_CUR )
		if err != nil { log.Fatal("Could not seek to 1Mb from end of first log file", err) }
		n, err = file.Read(buf)
		if err != nil { return err }
		ind = bytes.LastIndex(buf, []byte("\n"+cur_time_str))
	}
	file.Seek(int64(ind-n+1), os.SEEK_CUR)
	return err
}
