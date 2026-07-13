package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cleanAndExpandPath expands environment variables and an initial tilde for
// the current user, then cleans the resulting path.
func cleanAndExpandPath(path string) (string, error) {
	return cleanAndExpandPathWithHome(path, os.UserHomeDir)
}

func cleanAndExpandPathWithHome(
	path string,
	homeDir func() (string, error),
) (string, error) {
	if path == "" {
		return "", nil
	}

	var missingEnv string
	path = os.Expand(path, func(name string) string {
		value, ok := os.LookupEnv(name)
		if (!ok || value == "") && missingEnv == "" {
			missingEnv = name
		}
		return value
	})
	if missingEnv != "" {
		return "", fmt.Errorf(
			"expand logger path: environment variable %q is unset or empty",
			missingEnv,
		)
	}

	if !strings.HasPrefix(path, "~") {
		return filepath.Clean(path), nil
	}

	remainder := path[1:]
	if remainder != "" && !os.IsPathSeparator(remainder[0]) {
		return "", fmt.Errorf(
			"user-specific home paths are not supported: %q",
			path,
		)
	}

	home, err := homeDir()
	if err != nil {
		return "", fmt.Errorf("resolve current user home directory: %w", err)
	}
	if home == "" {
		return "", fmt.Errorf(
			"resolve current user home directory: empty path",
		)
	}

	for len(remainder) > 0 && os.IsPathSeparator(remainder[0]) {
		remainder = remainder[1:]
	}
	return filepath.Clean(filepath.Join(home, remainder)), nil
}
