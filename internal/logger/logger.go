package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

var (
	infoLog *log.Logger
	warnLog *log.Logger
	errLog  *log.Logger
	logFile *os.File
)

func init() {
	SetOutput(os.Stdout)
}

func Init(logPath string) error {
	if logPath == "" {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0750); err != nil {
		return err
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return fmt.Errorf("failed to open log file %s: %w", logPath, err)
	}

	logFile = f
	multi := io.MultiWriter(os.Stdout, f)
	SetOutput(multi)

	return nil
}

// isoNow returns a short ISO-8601 timestamp without zone info, e.g.
// "2026-09-25T05:09:41". The web log viewer parses exactly this shape.
func isoNow() string {
	return time.Now().Format("2006-01-02T15:04:05")
}

// levelWriter prefixes every log line with a bracketed level tag followed by a
// single space and the short ISO-8601 timestamp, e.g.
// "[INFO] 2026-09-25T05:09:41 message".
type levelWriter struct {
	level string
	out   io.Writer
}

func (lw levelWriter) Write(b []byte) (int, error) {
	if _, err := fmt.Fprintf(lw.out, "[%s] %s ", lw.level, isoNow()); err != nil {
		return 0, err
	}
	return lw.out.Write(b)
}

func SetOutput(w io.Writer) {
	infoLog = log.New(levelWriter{level: "INFO", out: w}, "", 0)
	warnLog = log.New(levelWriter{level: "WARN", out: w}, "", 0)
	errLog = log.New(levelWriter{level: "ERROR", out: w}, "", 0)
}

func Info(format string, v ...interface{}) {
	if infoLog != nil {
		infoLog.Printf(format, v...)
	}
}

func Warn(format string, v ...interface{}) {
	if warnLog != nil {
		warnLog.Printf(format, v...)
	}
}

func Error(format string, v ...interface{}) {
	if errLog != nil {
		errLog.Printf(format, v...)
	}
}

func Close() {
	if logFile != nil {
		_ = logFile.Close()
	}
}

// Reopen resets the output to stdout, closes the previous log file and opens
// logPath again. It is used on config reload so a rotated log file is re-opened
// and a new log path takes effect. On failure output falls back to stdout.
func Reopen(logPath string) error {
	SetOutput(os.Stdout)
	Close()
	return Init(logPath)
}
