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

package budget

import (
	"errors"
	"math"
	"testing"
)

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name    string
		current int64
		max     int64
		warnAt  int32
		state   State
		pct     int32
		err     error
	}{
		{name: "empty", current: 0, max: 5000, warnAt: 80, state: Within, pct: 0},
		{name: "half", current: 2500, max: 5000, warnAt: 80, state: Within, pct: 50},
		{name: "just under warn", current: 3999, max: 5000, warnAt: 80, state: Within, pct: 79},
		{name: "at warn", current: 4000, max: 5000, warnAt: 80, state: Warning, pct: 80},
		{name: "exactly at max", current: 5000, max: 5000, warnAt: 80, state: Warning, pct: 100},
		{name: "one over", current: 5001, max: 5000, warnAt: 80, state: Exceeded, pct: 100},
		{name: "well over", current: 7500, max: 5000, warnAt: 80, state: Exceeded, pct: 150},
		{name: "warn at 100", current: 4999, max: 5000, warnAt: 100, state: Within, pct: 99},
		{name: "too large for int32", current: math.MaxInt64, max: 1, warnAt: 80, state: Exceeded, pct: math.MaxInt32},
		{name: "zero max", current: 10, max: 0, warnAt: 80, err: ErrInvalidMax},
		{name: "negative max", current: 10, max: -1, warnAt: 80, err: ErrInvalidMax},
		{name: "warn zero", current: 10, max: 100, warnAt: 0, err: ErrInvalidWarnAt},
		{name: "warn too high", current: 10, max: 100, warnAt: 101, err: ErrInvalidWarnAt},
		{name: "negative count", current: -1, max: 100, warnAt: 80, err: ErrNegativeCount},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, pct, err := Evaluate(tt.current, tt.max, tt.warnAt)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("err = %v, want %v", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if state != tt.state || pct != tt.pct {
				t.Errorf("Evaluate(%d, %d, %d) = (%s, %d), want (%s, %d)",
					tt.current, tt.max, tt.warnAt, state, pct, tt.state, tt.pct)
			}
		})
	}
}
