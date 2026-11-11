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
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCountSeries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != labelValuesPath {
			t.Errorf("path = %q", r.URL.Path)
		}
		q := r.URL.Query()
		if got := q.Get("label_names[]"); got != "job" {
			t.Errorf("label_names[] = %q, want job", got)
		}
		if got := q.Get("selector"); got != `{job="avalanche-team-a"}` {
			t.Errorf("selector = %q", got)
		}
		if got := q.Get("count_method"); got != "active" {
			t.Errorf("count_method = %q, want active", got)
		}
		if got := r.Header.Get("X-Scope-OrgID"); got != testTenant {
			t.Errorf("X-Scope-OrgID = %q, want demo", got)
		}
		w.Header().Set("Content-Type", "application/json")
		// Shape captured from Mimir 3.2.4: series_count_total is the whole tenant's.
		_, _ = w.Write([]byte(`{"series_count_total": 18270, "labels": [{"label_name": "job",` +
			`"label_values_count": 1, "series_count": 512,` +
			`"cardinality": [{"label_value": "avalanche-team-a", "series_count": 512}]}]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Tenant: testTenant, HTTP: srv.Client()}
	got, err := c.CountSeries(context.Background(), "job", "avalanche-team-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 512 {
		t.Errorf("got %d, want 512", got)
	}
}

func TestNoTenantHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["X-Scope-Orgid"]; ok {
			t.Error("X-Scope-OrgID should not be sent when Tenant is empty")
		}
		_, _ = w.Write([]byte(`{"labels": []}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	if _, err := c.CountSeries(context.Background(), "job", "x"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "cardinality analysis is disabled for the tenant: demo", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := c.CountSeries(context.Background(), "job", "x")

	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want a *StatusError", err)
	}
	if se.Code != http.StatusBadRequest {
		t.Errorf("Code = %d, want 400", se.Code)
	}
}

func TestBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if _, err := c.CountSeries(context.Background(), "job", "x"); err == nil {
		t.Fatal("expected an error for invalid JSON")
	}
}

func TestContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	start := time.Now()
	_, err := c.CountSeries(ctx, "job", "x")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if time.Since(start) > time.Second {
		t.Error("CountSeries ignored the context deadline")
	}
}

func TestSelectorEscaping(t *testing.T) {
	tests := []struct {
		name, value, want string
	}{
		{"plain", "team-a", `{namespace="team-a"}`},
		{"double quote", `a"b`, `{namespace="a\"b"}`},
		{"backslash", `a\b`, `{namespace="a\\b"}`},
		{"injected matcher", `a",job="b`, `{namespace="a\",job=\"b"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.Query().Get("selector"); got != tt.want {
					t.Errorf("selector = %q, want %q", got, tt.want)
				}
				_, _ = w.Write([]byte(`{"labels": []}`))
			}))
			defer srv.Close()

			c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
			if _, err := c.CountSeries(context.Background(), "namespace", tt.value); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestBaseURLTrailingSlash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != labelValuesPath {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"labels": []}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/", HTTP: srv.Client()}
	if _, err := c.CountSeries(context.Background(), "job", "x"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResponseShapes(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    int64
		wantErr bool
	}{
		{name: "nothing matched is a real zero", body: `{"series_count_total": 18270, "labels": []}`, want: 0},
		{name: "reads the requested label", body: `{"labels": [{"label_name": "other", "series_count": 1},` +
			`{"label_name": "job", "series_count": 7}]}`, want: 7},
		{name: "total alone is not an answer", body: `{"series_count_total": 18270}`, wantErr: true},
		{name: "null labels", body: `{"labels": null}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
			got, err := c.CountSeries(context.Background(), "job", "x")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestStatusErrorBodyIsTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 2*maxErrorBody))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := c.CountSeries(context.Background(), "job", "x")

	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want a *StatusError", err)
	}
	if len(se.Body) != maxErrorBody {
		t.Errorf("len(Body) = %d, want %d", len(se.Body), maxErrorBody)
	}
}

func TestOversizedResponseFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte(" "), maxResponseBody+1))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := c.CountSeries(context.Background(), "job", "x")
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("err = %v, want one about the response size", err)
	}
}
