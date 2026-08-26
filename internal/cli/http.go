package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/version"
	"github.com/hellodword/vaultwarden-less/internal/worker"
	"github.com/spf13/cobra"
)

const maxWorkerResponseBytes = 1 << 20

func requestBackupCommand(environment commandEnvironment) *cobra.Command {
	return &cobra.Command{
		Use:   "request-backup",
		Short: "Queue one in-memory backup check demand",
		Args:  noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var result struct {
				SchemaVersion int  `json:"schema_version"`
				Accepted      bool `json:"accepted"`
			}
			if err := callWorker(cmd.Context(), environment, http.MethodPost, worker.CheckPath, http.StatusAccepted, &result); err != nil {
				return err
			}
			if result.SchemaVersion != version.SchemaVersion || !result.Accepted {
				return fmt.Errorf("worker returned an invalid acceptance response")
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
}

func statusCommand(environment commandEnvironment) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Print the current process-local worker status",
		Args:  noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var status worker.Status
			if err := callWorker(cmd.Context(), environment, http.MethodGet, worker.StatusPath, http.StatusOK, &status); err != nil {
				return err
			}
			if err := validateWorkerStatus(status); err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), status)
		},
	}
}

func healthcheckCommand(environment commandEnvironment) *cobra.Command {
	return &cobra.Command{
		Use:   "healthcheck",
		Short: "Check whether the internal worker HTTP process is alive",
		Args:  noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return callWorker(cmd.Context(), environment, http.MethodGet, worker.HealthPath, http.StatusNoContent, nil)
		},
	}
}

func callWorker(ctx context.Context, environment commandEnvironment, method, path string, expectedStatus int, destination any) error {
	endpoint, err := workerEndpoint(environment.workerURL, path)
	if err != nil {
		return err
	}
	var body io.Reader
	if method == http.MethodPost {
		body = bytes.NewReader([]byte("{\"schema_version\":1}\n"))
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("create worker request")
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := environment.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("worker request failed")
	}
	defer func() { _ = response.Body.Close() }()
	limited := io.LimitReader(response.Body, maxWorkerResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read worker response")
	}
	if len(data) > maxWorkerResponseBytes {
		return fmt.Errorf("worker response is too large")
	}
	if response.StatusCode != expectedStatus {
		return fmt.Errorf("worker endpoint returned status %d", response.StatusCode)
	}
	if destination == nil {
		if len(data) != 0 {
			return fmt.Errorf("worker returned an unexpected response body")
		}
		return nil
	}
	mediaType, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || len(parameters) != 0 {
		return fmt.Errorf("worker returned an invalid content type")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("worker returned invalid JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("worker returned trailing JSON")
	}
	return nil
}

func workerEndpoint(base, path string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return "", fmt.Errorf("worker URL is invalid")
	}
	parsed.Path = path
	return parsed.String(), nil
}

func validateWorkerStatus(status worker.Status) error {
	if status.SchemaVersion != version.SchemaVersion || !oneOf(status.State, "idle", "debouncing", "checking") || status.Targets == nil {
		return fmt.Errorf("worker returned an invalid status document")
	}
	if !validUTCTime(status.StartedAt) || status.NextCheckAt != nil && !validUTCTime(*status.NextCheckAt) {
		return fmt.Errorf("worker returned an invalid status timestamp")
	}
	if status.LastCheck != nil {
		last := status.LastCheck
		if !validUTCTime(last.StartedAt) || !validUTCTime(last.FinishedAt) || !oneOf(last.Result, "unchanged", "created", "partial", "failed") {
			return fmt.Errorf("worker returned an invalid last check")
		}
		if last.ContentID != nil && !snapshot.IsContentID(*last.ContentID) || last.ErrorCode != nil && !validErrorCode(*last.ErrorCode) {
			return fmt.Errorf("worker returned an invalid last check identifier")
		}
	}
	seen := map[string]struct{}{}
	for _, target := range status.Targets {
		if target.Name == "" || !oneOf(target.State, "unknown", "healthy", "retrying", "degraded") {
			return fmt.Errorf("worker returned an invalid target status")
		}
		if _, duplicate := seen[target.Name]; duplicate {
			return fmt.Errorf("worker returned duplicate target status")
		}
		seen[target.Name] = struct{}{}
		for _, value := range []*string{target.LastAttemptAt, target.LastSuccessAt} {
			if value != nil && !validUTCTime(*value) {
				return fmt.Errorf("worker returned an invalid target timestamp")
			}
		}
		if target.SnapshotID != nil && !validSnapshotID(*target.SnapshotID) || target.ContentID != nil && !snapshot.IsContentID(*target.ContentID) || target.ErrorCode != nil && !validErrorCode(*target.ErrorCode) {
			return fmt.Errorf("worker returned an invalid target identifier")
		}
	}
	return nil
}

func validUTCTime(value string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !parsed.IsZero() && strings.HasSuffix(value, "Z")
}

func validErrorCode(value string) bool {
	if value == "" || len(value) > 64 || strings.HasPrefix(value, "_") || strings.HasSuffix(value, "_") {
		return false
	}
	for _, character := range value {
		if character != '_' && (character < 'a' || character > 'z') {
			return false
		}
	}
	return true
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
