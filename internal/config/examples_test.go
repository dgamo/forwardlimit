// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/config"
)

// Every configuration under examples/ must load and build.
//
// A documentation example that no longer works is worse than none: it is the first
// thing a stranger copies, and a schema change that breaks one would otherwise go
// unnoticed until someone reports it.
func TestExampleConfigsLoadAndBuild(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..", "examples")

	var configs []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".yaml" {
			return nil
		}
		// Only forwardlimit's own configuration; the tree also holds Traefik,
		// Compose and Kubernetes manifests, which this schema knows nothing about.
		if isForwardlimitConfig(path) {
			configs = append(configs, path)
		}
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, configs, "expected to find example configurations under %s", root)

	for _, path := range configs {
		t.Run(filepath.ToSlash(path), func(t *testing.T) {
			t.Parallel()

			f, err := config.LoadFile(path)
			require.NoError(t, err)

			// Built with a secret, so an example using hash: true is exercised
			// rather than skipped with a warning.
			built, err := f.Build("example-secret")
			require.NoError(t, err)
			require.NotEmpty(t, built.Limiters, "every example should produce a limiter")
			require.Empty(t, built.Warnings)
		})
	}
}

// isForwardlimitConfig reports whether a YAML file is one of ours.
//
// Matching on location rather than content keeps the rule obvious: anything in
// examples/config, plus the file each deployment example mounts into the service.
func isForwardlimitConfig(path string) bool {
	dir, file := filepath.Split(filepath.ToSlash(path))

	switch {
	case dir == "../../examples/config/":
		return true
	case file == "forwardlimit.yaml":
		return true
	default:
		return false
	}
}
