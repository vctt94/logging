package logging

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// cleanAndExpandPath expands environment variables and an initial ~ or
// ~username, then cleans the resulting path.
func cleanAndExpandPath(path string) string {
	if path == "" {
		return path
	}

	path = os.ExpandEnv(path)
	if !strings.HasPrefix(path, "~") {
		return filepath.Clean(path)
	}

	path = path[1:]

	separators := string(os.PathSeparator)
	if runtime.GOOS == "windows" {
		separators += "/"
	}

	username := ""
	if i := strings.IndexAny(path, separators); i >= 0 {
		username = path[:i]
		path = path[i:]
	}

	var homeDir string

	var (
		currentUser *user.User
		err         error
	)

	if username == "" {
		currentUser, err = user.Current()
	} else {
		currentUser, err = user.Lookup(username)
	}

	if err == nil {
		homeDir = currentUser.HomeDir
	}

	if homeDir == "" {
		homeDir = "."
	}

	return filepath.Join(homeDir, path)
}
