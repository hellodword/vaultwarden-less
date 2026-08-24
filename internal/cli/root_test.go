package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestTopLevelCLIUsageAndDiscoveryContract(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		args       []string
		exitCode   int
		stdoutText string
	}{
		{name: "missing subcommand", exitCode: 2},
		{name: "unknown subcommand", args: []string{"unknown"}, exitCode: 2},
		{name: "restore required flags", args: []string{"restore"}, exitCode: 2},
		{name: "repo required subcommand", args: []string{"repo"}, exitCode: 2},
		{name: "help", args: []string{"--help"}, exitCode: 0, stdoutText: "backup.yaml"},
		{name: "version", args: []string{"--version"}, exitCode: 0, stdoutText: "1.0.0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			command := New(&stdout, &stderr)
			command.SetArgs(test.args)
			err := command.Execute()
			if got := ExitCode(err); got != test.exitCode {
				t.Fatalf("exit code = %d, want %d (error %v)", got, test.exitCode, err)
			}
			if test.stdoutText != "" && !strings.Contains(stdout.String(), test.stdoutText) {
				t.Fatalf("stdout %q does not contain %q", stdout.String(), test.stdoutText)
			}
		})
	}
}

func TestUnexpectedPositionalArgumentIsUsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"config", "unexpected"})
	if err := command.Execute(); ExitCode(err) != 2 {
		t.Fatalf("unexpected positional argument returned %v", err)
	}
}

func TestHelpDoesNotExposeRemovedPreflightCommand(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"--help"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "preflight") {
		t.Fatalf("removed command remains in help: %s", stdout.String())
	}
}
