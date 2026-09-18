package server

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

// healthzStart captures the daemon uptime used by the /healthz endpoint. It is
// intentionally kept separate from telemetry so the liveness probe stays cheap
// and dependency-free.
var healthzStart = time.Now()

// HandleHealthz serves an unauthenticated liveness probe used by orchestrators,
// load balancers, and uptime monitors. It reports 200 when the daemon can still
// write to its storage path and 503 otherwise.
func (s *Server) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	storageWritable := checkStorageWritable(s.cfg.StoragePath)

	w.Header().Set("Content-Type", "application/json")
	if !storageWritable {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"degraded","storage_writable":false,"uptime_seconds":` +
			fmt.Sprintf("%.2f", time.Since(healthzStart).Seconds()) + `}`))
		return
	}

	_, _ = w.Write([]byte(`{"status":"ok","storage_writable":true,"uptime_seconds":` +
		fmt.Sprintf("%.2f", time.Since(healthzStart).Seconds()) + `}`))
}

// checkStorageWritable verifies that the storage path is writable by creating
// and removing a temporary probe file.
func checkStorageWritable(dir string) bool {
	probe, err := os.CreateTemp(dir, ".healthz-*")
	if err != nil {
		return false
	}
	probeName := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probeName)
	return true
}
