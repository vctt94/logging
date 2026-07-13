package logging

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanAndExpandPath(t *testing.T) {
	t.Setenv("LOGGING_TEST_DIR", filepath.Join("var", "lib", "logging"))

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "empty", path: "", want: ""},
		{
			name: "environment and cleaning",
			path: strings.Join(
				[]string{"$LOGGING_TEST_DIR", "..", "app.log"},
				string(os.PathSeparator),
			),
			want: filepath.Join("var", "lib", "app.log"),
		},
		{
			name: "plain path cleaning",
			path: filepath.Join("logs", ".", "archive", "..", "app.log"),
			want: filepath.Join("logs", "app.log"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := cleanAndExpandPathWithHome(test.path, func() (string, error) {
				t.Fatal("home resolver called for a path without a tilde")
				return "", nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("path = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCleanAndExpandCurrentUserHome(t *testing.T) {
	home := filepath.Join(string(os.PathSeparator), "home", "logging")
	got, err := cleanAndExpandPathWithHome(
		filepath.Join("~", "logs", "..", "app.log"),
		func() (string, error) {
			return home, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "app.log")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestCleanAndExpandPathRejectsUnsetEnvironmentVariable(t *testing.T) {
	const name = "LOGGING_TEST_UNSET_ENV"

	previous, existed := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(name, previous)
		} else {
			_ = os.Unsetenv(name)
		}
	})

	_, err := cleanAndExpandPathWithHome(
		"$"+name+"/app.log",
		func() (string, error) {
			t.Fatal("home resolver called after environment expansion failed")
			return "", nil
		},
	)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), name) {
		t.Fatalf("error = %v, want variable name %q", err, name)
	}
}

func TestCleanAndExpandPathRejectsEmptyEnvironmentVariable(t *testing.T) {
	const name = "LOGGING_TEST_EMPTY_ENV"
	t.Setenv(name, "")

	_, err := cleanAndExpandPathWithHome(
		"$"+name+"/app.log",
		func() (string, error) {
			t.Fatal("home resolver called after environment expansion failed")
			return "", nil
		},
	)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), name) {
		t.Fatalf("error = %v, want variable name %q", err, name)
	}
}

func TestCleanAndExpandPathExpandsTildeFromEnvironment(t *testing.T) {
	t.Setenv("LOGGING_TEST_HOME_PATH", filepath.Join("~", "logs"))

	home := filepath.Join(string(os.PathSeparator), "home", "logging")
	got, err := cleanAndExpandPathWithHome(
		filepath.Join("$LOGGING_TEST_HOME_PATH", "app.log"),
		func() (string, error) {
			return home, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(home, "logs", "app.log")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestCleanAndExpandPathRejectsUnsupportedOrMissingHome(t *testing.T) {
	tests := []struct {
		name string
		path string
		home func() (string, error)
		want string
	}{
		{
			name: "named user",
			path: "~alice/logs/app.log",
			home: func() (string, error) {
				return "/unused", nil
			},
			want: "user-specific home paths are not supported",
		},
		{
			name: "empty home",
			path: "~/logs/app.log",
			home: func() (string, error) {
				return "", nil
			},
			want: "empty path",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := cleanAndExpandPathWithHome(test.path, test.home)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestCleanAndExpandPathWrapsHomeResolverError(t *testing.T) {
	resolveError := errors.New("home unavailable")

	_, err := cleanAndExpandPathWithHome("~/logs/app.log", func() (string, error) {
		return "", resolveError
	})
	if !errors.Is(err, resolveError) {
		t.Fatalf("error = %v, want wrapped error %v", err, resolveError)
	}
}

func TestNewLogBackendReportsPathExpansionFailure(t *testing.T) {
	_, err := NewLogBackend(LogConfig{
		LogFile:     "~unsupported/app.log",
		MaxLogFiles: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "expand log file path") {
		t.Fatalf("error = %v", err)
	}
}

func TestNewLogBackendReportsEnvironmentExpansionFailure(t *testing.T) {
	const name = "LOGGING_TEST_BACKEND_UNSET_ENV"

	previous, existed := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(name, previous)
		} else {
			_ = os.Unsetenv(name)
		}
	})

	_, err := NewLogBackend(LogConfig{
		LogFile:     "$" + name + "/app.log",
		MaxLogFiles: 1,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "expand log file path") {
		t.Fatalf("error = %v, want path expansion context", err)
	}
	if !strings.Contains(err.Error(), name) {
		t.Fatalf("error = %v, want variable name %q", err, name)
	}
}
