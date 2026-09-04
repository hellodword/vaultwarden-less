package restic

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/privatefs"
)

func (r Runner) targetCommand(target config.Target) ([]string, []string, error) {
	if err := target.Validate(); err != nil {
		return nil, nil, &OperationError{Code: "target_invalid", ExitCode: -1}
	}
	if r.credentials == nil {
		return nil, nil, &OperationError{Code: "credentials_missing", ExitCode: -1}
	}
	credentialEnvironment, err := r.credentials.Environment(target)
	if err != nil {
		return nil, nil, &OperationError{Code: "credentials_invalid", ExitCode: -1}
	}
	if err := privatefs.ValidatePrivateDirectory(r.passwordRoot); err != nil {
		return nil, nil, &OperationError{Code: "password_path_invalid", ExitCode: -1}
	}
	passwordFile := filepath.Join(r.passwordRoot, target.Name)
	if filepath.Dir(passwordFile) != filepath.Clean(r.passwordRoot) {
		return nil, nil, &OperationError{Code: "password_path_invalid", ExitCode: -1}
	}
	if _, err := config.ReadPassword(r.passwordRoot, target.Name); err != nil {
		return nil, nil, &OperationError{Code: "password_file_invalid", ExitCode: -1}
	}
	if err := privatefs.ValidatePrivateDirectory(r.cacheDir); err != nil {
		return nil, nil, &OperationError{Code: "cache_path_invalid", ExitCode: -1}
	}
	cache := filepath.Join(r.cacheDir, target.Name)
	if filepath.Dir(cache) != filepath.Clean(r.cacheDir) {
		return nil, nil, &OperationError{Code: "cache_path_invalid", ExitCode: -1}
	}
	if err := privatefs.EnsurePrivateDirectory(cache); err != nil {
		return nil, nil, &OperationError{Code: "cache_create_failed", ExitCode: -1}
	}

	base := []string{
		"--repo", target.Repository,
		"--password-file", passwordFile,
		"--cache-dir", cache,
	}
	for _, option := range target.Options {
		base = append(base, "--option", option.Name+"="+option.Value)
	}
	return base, sanitizedEnvironment(credentialEnvironment), nil
}

func sanitizedEnvironment(credentials map[string]string) []string {
	environment := []string{
		"HOME=/nonexistent",
		"SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt",
		"TMPDIR=/tmp",
	}
	names := make([]string, 0, len(credentials))
	for name := range credentials {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		environment = append(environment, name+"="+credentials[name])
	}
	return environment
}

func (r Runner) runTargetCommand(
	ctx context.Context,
	target config.Target,
	operation string,
	arguments, environment []string,
	directory string,
) ([]byte, error) {
	return r.runLogged(ctx, operation, target.Name, r.binary, arguments, environment, directory)
}

func (r Runner) runLogged(
	ctx context.Context,
	operation, target, binary string,
	arguments, environment []string,
	directory string,
) ([]byte, error) {
	started := time.Now()
	output, err := r.run(ctx, binary, arguments, environment, directory)
	if r.logger == nil {
		return output, err
	}
	attributes := []any{
		"operation", operation,
		"duration_ms", time.Since(started).Milliseconds(),
		"exit_code", operationExitCode(err),
	}
	if target != "" {
		attributes = append(attributes, "target", target)
	}
	if err == nil {
		r.logger.Info("restic operation completed", attributes...)
	} else {
		var operationError *OperationError
		if errors.As(err, &operationError) {
			attributes = append(attributes, "error_code", operationError.Code)
		}
		r.logger.Error("restic operation failed", attributes...)
	}
	return output, err
}

func operationExitCode(err error) int {
	if err == nil {
		return 0
	}
	var operationError *OperationError
	if errors.As(err, &operationError) {
		return operationError.ExitCode
	}
	return -1
}
