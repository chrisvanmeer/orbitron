package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

// Setup initializes logging to both stdout (systemd) and the specified log file
func Setup(logPath string) (*os.File, error) {
	log.SetFlags(log.Ldate | log.Ltime)

	if logPath == "" {
		return nil, nil
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file (%s): %w", logPath, err)
	}

	// Output logs simultaneously to stdout (journalctl) and log file
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multiWriter)

	return logFile, nil
}

func Info(format string, v ...any) {
	log.Printf("[INFO] "+format, v...)
}

func Warn(format string, v ...any) {
	log.Printf("[WARN] "+format, v...)
}

func Error(format string, v ...any) {
	log.Printf("[ERROR] "+format, v...)
}

func Fatal(format string, v ...any) {
	log.Fatalf("[FATAL] "+format, v...)
}
