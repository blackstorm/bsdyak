package cli

import (
	"bytes"
	"testing"
)

func TestAllSubcommandsInitializeWithoutFlagCollisions(t *testing.T) {
	for _, args := range [][]string{
		{"--help"},
		{"deploy", "--help"},
		{"down", "--help"},
		{"ps", "--help"},
		{"logs", "--help"},
		{"exec", "--help"},
		{"secret", "--help"},
		{"secret", "set", "--help"},
		{"secret", "unset", "--help"},
		{"secret", "ls", "--help"},
		{"server", "--help"},
		{"server", "init", "--help"},
		{"server", "check", "--help"},
		{"server", "run", "--help"},
		{"bridge", "--help"},
		{"version"},
	} {
		root := NewRootCmd()
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetErr(&buf)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, buf.String())
		}
	}
}
