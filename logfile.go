package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"k8s.io/utils/inotify"
)

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

func (f *logFile) Read(buf []byte) (n int, err error) {
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

	switch n = copy(buf, f.tailBuf); {
	case n == len(f.tailBuf):
		f.tailBuf = nil
	case n == len(buf):
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
				buf := make([]byte, bufSize)
				num, err := f.File.Read(buf)
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
				f.blockChan <- buf[0:num:num]
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
	log_file := &logFile{
		File:      file,
		cacheSize: cacheSize,
		tailBuf:   make([]byte, 0, bufSize),
		watcher:   w,
	}
	return log_file, err
}
