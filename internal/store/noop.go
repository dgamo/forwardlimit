// SPDX-License-Identifier: Apache-2.0

package store

import "context"

// Noop allows everything. Installed when limiting is switched off, so nothing else
// needs to special-case the disabled state.
type Noop struct{}

// Consume implements Store.
func (Noop) Consume(context.Context, Key, Rule) (Result, error) { return Result{}, nil }

// Take implements Store.
func (Noop) Take(_ context.Context, _ Key, b Bucket) (TakeResult, error) {
	return TakeResult{Allowed: true, Remaining: b.Burst}, nil
}

// Close implements Store.
func (Noop) Close() error { return nil }
