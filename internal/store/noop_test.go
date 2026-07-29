// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/store"
)

// Noop is installed when limiting is switched off, so it must allow everything
// unconditionally and never report an error.
func TestNoopAllowsEverything(t *testing.T) {
	t.Parallel()

	var s store.Store = store.Noop{}
	rule := store.Rule{Limit: 1, Window: time.Minute, Block: time.Minute}

	for i := 0; i < 10; i++ {
		res, err := s.Consume(context.Background(), store.Key{Limiter: "login", Bucket: "abc"}, rule)
		require.NoError(t, err)
		require.False(t, res.Blocked)
		require.Zero(t, res.Count)
	}
	require.NoError(t, s.Close())
}
