//go:build integration

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

package mimir

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// Run it with make test-integration, which starts the local stack.
func TestCountSeriesAgainstMimir(t *testing.T) {
	base := os.Getenv("KARDIGAN_MIMIR_URL")
	if base == "" {
		t.Skip("KARDIGAN_MIMIR_URL not set")
	}
	c := &Client{BaseURL: base, Tenant: "demo", HTTP: &http.Client{Timeout: 10 * time.Second}}

	// Avalanche's flags fix these counts (see hack/local-stack/docker-compose.yaml),
	// and Prometheus adds a few of its own per job. Checking them, not just
	// n != 0, is what catches a count that isn't scoped to the team.
	tests := []struct {
		job  string
		want int64
	}{
		{"avalanche-team-a", 500},
		{"avalanche-team-b", 2000},
		{"avalanche-team-c", 15750},
		{"avalanche-team-d", 1000},
		{"does-not-exist", 0},
	}
	for _, tt := range tests {
		n, err := c.CountSeries(context.Background(), "job", tt.job)
		if err != nil {
			t.Fatalf("%s: %v", tt.job, err)
		}
		if n < tt.want || n > tt.want+tt.want/20 {
			t.Errorf("%s: %d series, want about %d; is the stack running and has it been up for two minutes?",
				tt.job, n, tt.want)
		}
		t.Logf("%s: %d active series", tt.job, n)
	}
}
