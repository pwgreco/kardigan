/*
Copyright 2026 Paul Greco.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"testing"
	"time"
)

func TestValidateUpstreams(t *testing.T) {
	const (
		mimirURL   = "http://mimir:8080"
		grafanaURL = "https://grafana.example.com"
		timeout    = 10 * time.Second
	)
	tests := []struct {
		name           string
		mimir, grafana string
		mimirTimeout   time.Duration
		grafanaTimeout time.Duration
		wantErr        bool
	}{
		{"both set", mimirURL, grafanaURL, timeout, timeout, false},
		{"no grafana", mimirURL, "", timeout, timeout, false},
		{"empty mimir", "", grafanaURL, timeout, timeout, true},
		{"mimir without a scheme", "mimir:8080", "", timeout, timeout, true},
		{"mimir without a host", "http://", "", timeout, timeout, true},
		{"unsupported scheme", "file:///etc/passwd", "", timeout, timeout, true},
		{"grafana unparseable", mimirURL, "http://[bad", timeout, timeout, true},
		{"credentials in the URL", mimirURL, "https://admin:secret@grafana", timeout, timeout, true},
		{"mimir timeout of zero", mimirURL, "", 0, timeout, true},
		{"negative grafana timeout", mimirURL, grafanaURL, timeout, -time.Second, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateUpstreams(tt.mimir, tt.grafana, tt.mimirTimeout, tt.grafanaTimeout)
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
