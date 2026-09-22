package httputil

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		headers    map[string]string
		remoteAddr string
		want       string
	}{
		{
			name:       "X-Real-IP wins over X-Forwarded-For",
			headers:    map[string]string{"X-Real-IP": "203.0.113.7", "X-Forwarded-For": "198.51.100.9, 203.0.113.7"},
			remoteAddr: "127.0.0.1:52345",
			want:       "203.0.113.7",
		},
		{
			name:       "X-Forwarded-For takes leftmost of chain",
			headers:    map[string]string{"X-Forwarded-For": "198.51.100.9, 10.0.0.1"},
			remoteAddr: "127.0.0.1:52345",
			want:       "198.51.100.9",
		},
		{
			name:       "X-Forwarded-For single value",
			headers:    map[string]string{"X-Forwarded-For": "198.51.100.9"},
			remoteAddr: "127.0.0.1:52345",
			want:       "198.51.100.9",
		},
		{
			name:       "RemoteAddr fallback strips port",
			remoteAddr: "203.0.113.8:443",
			want:       "203.0.113.8",
		},
		{
			name:       "RemoteAddr fallback raw",
			remoteAddr: "203.0.113.8",
			want:       "203.0.113.8",
		},
		{
			name:       "blank forwarded headers fall back to RemoteAddr",
			headers:    map[string]string{"X-Real-IP": "", "X-Forwarded-For": ""},
			remoteAddr: "127.0.0.1:52345",
			want:       "127.0.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://mirror.example.com/api/", nil)
			req.RemoteAddr = tt.remoteAddr
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			if got := ClientIP(req); got != tt.want {
				t.Errorf("ClientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}