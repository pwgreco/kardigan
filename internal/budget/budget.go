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

// Package budget decides whether a series count is within, near or over a budget.
package budget

import (
	"errors"
	"math"
)

// State is the outcome of evaluating a budget.
type State string

// The states a budget can be in. Their values appear as condition reasons.
const (
	Within   State = "WithinBudget"
	Warning  State = "WarningThreshold"
	Exceeded State = "BudgetExceeded"
)

// Sentinel errors let callers check the cause with errors.Is.
var (
	ErrInvalidMax    = errors.New("max must be greater than zero")
	ErrInvalidWarnAt = errors.New("warnAt must be between 1 and 100")
	ErrNegativeCount = errors.New("current must not be negative")
)

// Evaluate returns the budget state and the usage percentage (rounded down).
// current > max is Exceeded (exactly at max is not), a percentage >= warnAt
// is Warning, and anything else is Within. The percentage isn't capped at 100
// (7500 of 5000 is 150), only at what an int32 can hold.
func Evaluate(current, max int64, warnAt int32) (State, int32, error) {
	if current < 0 {
		return "", 0, ErrNegativeCount
	}
	if max <= 0 {
		return "", 0, ErrInvalidMax
	}
	if warnAt < 1 || warnAt > 100 {
		return "", 0, ErrInvalidWarnAt
	}

	pct := percent(current, max)
	switch {
	case current > max:
		return Exceeded, pct, nil
	case pct >= warnAt:
		return Warning, pct, nil
	default:
		return Within, pct, nil
	}
}

// percent returns current as a percentage of max, rounded down, capped at what an
// int32 can hold. It divides first, because current*100 could overflow.
func percent(current, max int64) int32 {
	whole := current / max
	if whole >= math.MaxInt32/100 {
		return math.MaxInt32
	}
	return int32(whole*100 + current%max*100/max) //nolint:gosec // the check above keeps it in range
}
