package restic

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"time"
)

func (r Runner) run(
	ctx context.Context,
	binary string,
	arguments, environment []string,
	directory string,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &OperationError{Code: "timeout", ExitCode: -1}
		}
		return nil, &OperationError{Code: "cancelled", ExitCode: -1}
	}
	if r.execute != nil {
		return r.execute(ctx, binary, slices.Clone(arguments), slices.Clone(environment), directory)
	}
	command := exec.Command(binary, arguments...)
	command.Dir = directory
	command.Env = slices.Clone(environment)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout := newBoundedWriter(maxCommandOutput)
	stderr := newBoundedWriter(maxStderrOutput)
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return nil, &OperationError{Code: "command_failed", ExitCode: -1}
	}

	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()

	cancelled := false
	var waitError error
	select {
	case waitError = <-waited:
	case <-ctx.Done():
		cancelled = true
		terminateProcessGroup(command.Process, syscall.SIGTERM)
		timer := time.NewTimer(2 * time.Second)
		select {
		case waitError = <-waited:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			terminateProcessGroup(command.Process, syscall.SIGKILL)
			waitError = <-waited
		}
	}
	if cancelled {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &OperationError{Code: "timeout", ExitCode: -1}
		}
		return nil, &OperationError{Code: "cancelled", ExitCode: -1}
	}
	exitCode := -1
	var exitError *exec.ExitError
	if errors.As(waitError, &exitError) {
		exitCode = exitError.ExitCode()
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, &OperationError{Code: "output_limit", ExitCode: exitCode}
	}
	if waitError != nil {
		return nil, &OperationError{Code: "command_failed", ExitCode: exitCode}
	}
	return stdout.data, nil
}

func terminateProcessGroup(process *os.Process, signal syscall.Signal) {
	if process == nil {
		return
	}
	if err := syscall.Kill(-process.Pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = process.Signal(signal)
	}
}

type boundedWriter struct {
	data     []byte
	limit    int
	exceeded bool
}

func newBoundedWriter(limit int) boundedWriter {
	return boundedWriter{data: make([]byte, 0, min(limit, 64<<10)), limit: limit}
}

func (writer *boundedWriter) Write(data []byte) (int, error) {
	remaining := writer.limit - len(writer.data)
	if remaining > 0 {
		writer.data = append(writer.data, data[:min(remaining, len(data))]...)
	}
	if len(data) > remaining {
		writer.exceeded = true
	}
	return len(data), nil
}
