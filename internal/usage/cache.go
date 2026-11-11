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
	"fmt"
	"sync"
	"time"
)

// Source supplies PromQL expressions, such as those in dashboards or rules.
type Source struct {
	Name  string
	Fetch func(ctx context.Context) ([]string, error)
}

// Snapshot is what every source's expressions read at one moment.
type Snapshot struct {
	Usage *Usage
	// Expressions counts the expressions fetched, by source name.
	Expressions map[string]int
	// ParseFailures counts expressions that could not be parsed. What they read
	// is unknown.
	ParseFailures int
}

// Total returns the number of expressions fetched from all sources.
func (s *Snapshot) Total() int {
	total := 0
	for _, n := range s.Expressions {
		total += n
	}
	return total
}

// Collect fetches every source and parses the results. It fails if any source
// fails: a metric missing from a dashboard Kardigan couldn't read isn't unused.
func Collect(ctx context.Context, sources []Source) (*Snapshot, error) {
	snapshot := &Snapshot{Usage: New(), Expressions: map[string]int{}}
	for _, source := range sources {
		exprs, err := source.Fetch(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", source.Name, err)
		}
		snapshot.Expressions[source.Name] = len(exprs)
		for _, expr := range exprs {
			if err := snapshot.Usage.Add(expr); err != nil {
				snapshot.ParseFailures++
			}
		}
	}
	return snapshot, nil
}

// Cache holds one Snapshot for all budgets, because dashboards and rules aren't
// specific to a team and refetching them per budget would be wasted work.
type Cache struct {
	Sources []Source
	TTL     time.Duration

	mu        sync.Mutex
	snapshot  *Snapshot
	fetchedAt time.Time
}

// Get returns the cached Snapshot, collecting a new one if it has expired. The
// lock is held while collecting, so concurrent callers share one fetch. Failures
// aren't cached.
func (c *Cache) Get(ctx context.Context) (*Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.snapshot != nil && time.Since(c.fetchedAt) < c.TTL {
		return c.snapshot, nil
	}
	snapshot, err := Collect(ctx, c.Sources)
	if err != nil {
		return nil, err
	}
	c.snapshot, c.fetchedAt = snapshot, time.Now()
	return snapshot, nil
}
