package main

import (
	"runtime"
	"strings"
	"testing"
)

func TestDispatch(t *testing.T) {
	tests := []struct {
		name string
		args []string
		exit int
	}{
		{"missing subcommand", nil, 2},
		{"unknown word", []string{"frobnicate"}, 2},
		{"-h is the unknown-word case", []string{"-h"}, 2},
		{"version", []string{"version"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			if got := run(tt.args, &stdout, &stderr); got != tt.exit {
				t.Fatalf("exit = %d, want %d", got, tt.exit)
			}
			if tt.exit == 2 {
				if stderr.String() != usage {
					t.Errorf("stderr = %q, want the usage block", stderr.String())
				}
				if stdout.String() != "" {
					t.Errorf("stdout = %q, want empty", stdout.String())
				}
			}
		})
	}
}

func TestVersionOutput(t *testing.T) {
	var stdout, stderr strings.Builder
	if got := run([]string{"version"}, &stdout, &stderr); got != 0 {
		t.Fatalf("exit = %d, want 0", got)
	}
	out := stdout.String()
	if want := "nag dev " + runtime.Version() + "\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}
