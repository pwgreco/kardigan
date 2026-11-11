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

package usage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func fixed(exprs ...string) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) { return exprs, nil }
}

func TestCollect(t *testing.T) {
	snapshot, err := Collect(context.Background(), []Source{
		{Name: "one", Fetch: fixed(aTotal, "rate(")},
		{Name: "two", Fetch: fixed("b_total")},
		{Name: "empty", Fetch: fixed()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Total(); got != 3 {
		t.Errorf("Total() = %d, want 3", got)
	}
	if snapshot.ParseFailures != 1 {
		t.Errorf("ParseFailures = %d, want 1", snapshot.ParseFailures)
	}
	if _, ok := snapshot.Expressions["empty"]; !ok {
		t.Error("a source that was read but empty should still be recorded")
	}
	if !snapshot.Usage.MetricUsed(aTotal) || !snapshot.Usage.MetricUsed("b_total") {
		t.Error("metrics from both sources should be used")
	}
}

func TestCollectFailsIfAnySourceFails(t *testing.T) {
	boom := errors.New("boom")
	_, err := Collect(context.Background(), []Source{
		{Name: "good", Fetch: fixed(aTotal)},
		{Name: "bad", Fetch: func(context.Context) ([]string, error) { return nil, boom }},
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap %v", err, boom)
	}
}

func TestCache(t *testing.T) {
	calls := 0
	var failure error
	cache := &Cache{TTL: time.Hour, Sources: []Source{{Name: "s", Fetch: func(context.Context) ([]string, error) {
		calls++
		return []string{aTotal}, failure
	}}}}
	ctx := context.Background()

	first, err := cache.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || calls != 1 {
		t.Errorf("second Get should use the cache: calls = %d", calls)
	}

	cache.fetchedAt = time.Now().Add(-2 * time.Hour)
	failure = errors.New("boom")
	if _, err := cache.Get(ctx); err == nil {
		t.Fatal("an expired cache should refetch, and report the failure")
	}
	if _, err := cache.Get(ctx); err == nil || calls != 3 {
		t.Errorf("a failure shouldn't be cached or hide the old result: calls = %d, err = %v", calls, err)
	}

	failure = nil
	if _, err := cache.Get(ctx); err != nil || calls != 4 {
		t.Errorf("should recover: calls = %d, err = %v", calls, err)
	}
}
