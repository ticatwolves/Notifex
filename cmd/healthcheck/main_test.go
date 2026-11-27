package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProbe(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()

	tests := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{name: "200 passes", target: ok.URL + "/healthz"},
		{name: "503 fails", target: bad.URL + "/healthz", wantErr: true},
		{name: "remote host refused", target: "http://169.254.169.254/latest/meta-data", wantErr: true},
		{name: "https refused", target: "https://127.0.0.1:9100/healthz", wantErr: true},
		{name: "garbage refused", target: "::not a url", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := probe(tt.target)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
