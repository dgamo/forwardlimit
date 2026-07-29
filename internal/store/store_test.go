// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/store"
)

func TestRuleValid(t *testing.T) {
	t.Parallel()

	valid := store.Rule{Limit: 5, Window: time.Minute, Block: time.Minute}

	tests := []struct {
		name string
		rule store.Rule
		want bool
	}{
		{"fully populated", valid, true},
		{"zero limit disables", store.Rule{Limit: 0, Window: time.Minute, Block: time.Minute}, false},
		{"zero window disables", store.Rule{Limit: 5, Window: 0, Block: time.Minute}, false},
		{"zero block disables", store.Rule{Limit: 5, Window: time.Minute, Block: 0}, false},
		{"negative limit disables", store.Rule{Limit: -1, Window: time.Minute, Block: time.Minute}, false},
		{"zero value disables", store.Rule{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, tt.rule.Valid())
		})
	}
}

func TestSanitizeBucket(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want string
	}{
		{"deadbeef", "deadbeef"},
		{"192.0.2.1", "192.0.2.1"},
		{"2001:db8::1", "2001:db8::1"},
		{"ab{cd}ef", "abcdef"},
		{"{}", ""},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, store.SanitizeBucket(tt.in))
		})
	}
}

func TestKeyString(t *testing.T) {
	t.Parallel()
	require.Equal(t, "login:abc123", store.Key{Limiter: "login", Bucket: "abc123"}.String())
}
