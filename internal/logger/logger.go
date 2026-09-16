package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
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

	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return err
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file %s: %w", logPath, err)
	}

	logFile = f
	multi := io.MultiWriter(os.Stdout, f)
	SetOutput(multi)

	return nil
}

func SetOutput(w io.Writer) {
	infoLog = log.New(w, "[INFO] ", log.LstdFlags)
	warnLog = log.New(w, "[WARN] ", log.LstdFlags)
	errLog = log.New(w, "[ERROR] ", log.LstdFlags)
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
