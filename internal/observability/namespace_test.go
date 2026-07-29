package observability_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/observability"
)

// Namespace() must report what is actually emitted, not what was asked for: the
// Prometheus client rewrites invalid characters regardless, and a log line or
// -validate output that disagrees with the exposition is misleading.
func TestNamespaceIsSanitisedToWhatIsEmitted(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"forwardlimit": "forwardlimit",
		"my-service":   "my_service",
		"my.service":   "my_service",
		"my service":   "my_service",
		"9lives":       "_lives",
		"a9":           "a9",
		"":             observability.DefaultNamespace,
		"  ":           observability.DefaultNamespace,
	} {
		require.Equal(t, want, observability.NewMetrics(in).Namespace(), "input %q", in)
	}
}
