package restic

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hellodword/vaultwarden-less/internal/strictjson"
)

var resticVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)

type rawSnapshot struct {
	ID       string   `json:"id"`
	Time     string   `json:"time"`
	Hostname string   `json:"hostname"`
	Tags     []string `json:"tags"`
	Paths    []string `json:"paths"`
}

func parseSnapshots(data []byte, expectedPath string) ([]Snapshot, error) {
	frames, err := decodeJSONFrames(data)
	if err != nil || len(frames) != 1 {
		return nil, &OperationError{Code: "snapshots_result_invalid", ExitCode: -1}
	}
	var raw []rawSnapshot
	if err := strictUnmarshal(frames[0], &raw); err != nil {
		return nil, &OperationError{Code: "snapshots_result_invalid", ExitCode: -1}
	}
	result := make([]Snapshot, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, item := range raw {
		if !validSnapshotID(item.ID) || item.Hostname != SnapshotHost || !slices.Equal(item.Paths, []string{expectedPath}) {
			return nil, &OperationError{Code: "snapshots_result_invalid", ExitCode: -1}
		}
		when, err := time.Parse(time.RFC3339Nano, item.Time)
		if err != nil || when.IsZero() {
			return nil, &OperationError{Code: "snapshots_result_invalid", ExitCode: -1}
		}
		identity, ok := snapshotIdentity(item.Tags)
		if !ok {
			return nil, &OperationError{Code: "snapshots_result_invalid", ExitCode: -1}
		}
		if _, duplicate := seen[item.ID]; duplicate {
			return nil, &OperationError{Code: "snapshots_result_invalid", ExitCode: -1}
		}
		seen[item.ID] = struct{}{}
		result = append(result, Snapshot{
			ID:            item.ID,
			Time:          formatTime(when),
			ProjectCommit: identity.projectCommit,
			ContentID:     identity.contentID,
			ArchiveID:     identity.archiveID,
		})
	}
	if result == nil {
		result = []Snapshot{}
	}
	return result, nil
}

type identityTags struct {
	projectCommit string
	contentID     string
	archiveID     string
}

func snapshotIdentity(tags []string) (identityTags, bool) {
	var result identityTags
	for _, tag := range tags {
		switch {
		case strings.HasPrefix(tag, "project_commit="):
			if result.projectCommit != "" {
				return identityTags{}, false
			}
			result.projectCommit = strings.TrimPrefix(tag, "project_commit=")
		case strings.HasPrefix(tag, "content_id="):
			if result.contentID != "" {
				return identityTags{}, false
			}
			result.contentID = strings.TrimPrefix(tag, "content_id=")
		case strings.HasPrefix(tag, "archive_id="):
			if result.archiveID != "" {
				return identityTags{}, false
			}
			result.archiveID = strings.TrimPrefix(tag, "archive_id=")
		}
	}
	return result, validCommit(result.projectCommit) && validDigest(result.contentID) && validDigest(result.archiveID)
}

func parseBackupSnapshotID(data []byte) (string, error) {
	frames, err := decodeJSONFrames(data)
	if err != nil || len(frames) == 0 {
		return "", fmt.Errorf("invalid backup output")
	}
	snapshotID := ""
	for index, frame := range frames {
		var message struct {
			MessageType string `json:"message_type"`
			SnapshotID  string `json:"snapshot_id"`
		}
		if err := strictUnmarshal(frame, &message); err != nil {
			return "", err
		}
		if message.MessageType == "summary" {
			if snapshotID != "" || index != len(frames)-1 {
				return "", fmt.Errorf("multiple backup summaries")
			}
			snapshotID = message.SnapshotID
		} else if snapshotID != "" || message.MessageType == "error" {
			return "", fmt.Errorf("invalid backup record order")
		}
	}
	if !validSnapshotID(snapshotID) {
		return "", fmt.Errorf("invalid backup summary")
	}
	return snapshotID, nil
}

type forgetGroup struct {
	Host   string           `json:"host"`
	Paths  []string         `json:"paths"`
	Keep   []forgetSnapshot `json:"keep"`
	Remove []forgetSnapshot `json:"remove"`
}

type forgetSnapshot struct {
	ID string `json:"id"`
}

func parseForgetGroups(data []byte, expectedPath string) ([]PruneGroup, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return []PruneGroup{}, nil
	}
	frames, err := decodeJSONFrames(data)
	if err != nil || len(frames) != 1 {
		return nil, fmt.Errorf("invalid forget output")
	}
	var raw []forgetGroup
	if err := strictUnmarshal(frames[0], &raw); err != nil {
		return nil, err
	}
	groups := make([]PruneGroup, 0, len(raw))
	for _, group := range raw {
		if group.Host != SnapshotHost || !slices.Equal(group.Paths, []string{expectedPath}) {
			return nil, fmt.Errorf("unexpected forget group")
		}
		parsed := PruneGroup{
			Host:              group.Host,
			Paths:             slices.Clone(group.Paths),
			KeepSnapshotIDs:   []string{},
			RemoveSnapshotIDs: []string{},
		}
		seen := map[string]struct{}{}
		for _, entry := range group.Keep {
			if !validSnapshotID(entry.ID) {
				return nil, fmt.Errorf("invalid kept snapshot ID")
			}
			if _, duplicate := seen[entry.ID]; duplicate {
				return nil, fmt.Errorf("duplicate snapshot ID")
			}
			seen[entry.ID] = struct{}{}
			parsed.KeepSnapshotIDs = append(parsed.KeepSnapshotIDs, entry.ID)
		}
		for _, entry := range group.Remove {
			if !validSnapshotID(entry.ID) {
				return nil, fmt.Errorf("invalid removed snapshot ID")
			}
			if _, duplicate := seen[entry.ID]; duplicate {
				return nil, fmt.Errorf("duplicate snapshot ID")
			}
			seen[entry.ID] = struct{}{}
			parsed.RemoveSnapshotIDs = append(parsed.RemoveSnapshotIDs, entry.ID)
		}
		groups = append(groups, parsed)
	}
	if groups == nil {
		groups = []PruneGroup{}
	}
	return groups, nil
}

func parseInitialized(data []byte) error {
	frames, err := decodeJSONFrames(data)
	if err != nil || len(frames) != 1 {
		return fmt.Errorf("invalid init output")
	}
	var result struct {
		MessageType string `json:"message_type"`
		ID          string `json:"id"`
	}
	if err := strictUnmarshal(frames[0], &result); err != nil || result.MessageType != "initialized" || !validSnapshotID(result.ID) {
		return fmt.Errorf("invalid init result")
	}
	return nil
}

func parseFinalSummary(data []byte, operation string) error {
	frames, err := decodeJSONFrames(data)
	if err != nil || len(frames) == 0 {
		return fmt.Errorf("missing final summary")
	}
	seen := false
	for index, frame := range frames {
		var header struct {
			MessageType string `json:"message_type"`
		}
		if err := strictUnmarshal(frame, &header); err != nil {
			return err
		}
		if header.MessageType == "error" || seen {
			return fmt.Errorf("invalid result record")
		}
		if header.MessageType != "summary" {
			continue
		}
		if index != len(frames)-1 {
			return fmt.Errorf("output follows final summary")
		}
		seen = true
		switch operation {
		case "check":
			var summary struct {
				MessageType string `json:"message_type"`
				NumErrors   int    `json:"num_errors"`
			}
			if err := strictUnmarshal(frame, &summary); err != nil || summary.NumErrors != 0 {
				return fmt.Errorf("check did not report success")
			}
		case "restore":
			var summary struct {
				MessageType   string `json:"message_type"`
				TotalFiles    uint64 `json:"total_files"`
				FilesRestored uint64 `json:"files_restored"`
				FilesSkipped  uint64 `json:"files_skipped"`
				TotalBytes    uint64 `json:"total_bytes"`
				BytesRestored uint64 `json:"bytes_restored"`
				BytesSkipped  uint64 `json:"bytes_skipped"`
			}
			if err := strictUnmarshal(frame, &summary); err != nil || summary.TotalFiles == 0 ||
				summary.FilesRestored != summary.TotalFiles || summary.FilesSkipped != 0 ||
				summary.BytesRestored != summary.TotalBytes || summary.BytesSkipped != 0 {
				return fmt.Errorf("restore did not report complete success")
			}
		default:
			return fmt.Errorf("unknown summary operation")
		}
	}
	if !seen {
		return fmt.Errorf("missing final summary")
	}
	return nil
}

func decodeJSONFrames(data []byte) ([]json.RawMessage, error) {
	if len(data) > maxCommandOutput || !utf8.Valid(data) {
		return nil, fmt.Errorf("JSON output exceeds bounds or is not UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	frames := make([]json.RawMessage, 0, 1)
	for {
		var frame json.RawMessage
		before := decoder.InputOffset()
		err := decoder.Decode(&frame)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || len(frame) == 0 || len(frame) > maxJSONFrame || decoder.InputOffset()-before > int64(maxJSONFrame) {
			return nil, fmt.Errorf("invalid or oversized JSON frame")
		}
		frames = append(frames, slices.Clone(frame))
		if len(frames) > maxJSONFrames {
			return nil, fmt.Errorf("too many JSON frames")
		}
	}
	return frames, nil
}

func strictUnmarshal(data []byte, destination any) error {
	if err := strictjson.RejectDuplicateKeys(data); err != nil {
		return err
	}
	return json.Unmarshal(data, destination)
}

func formatTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func validCommit(value string) bool {
	return len(value) == 40 && validHex(value)
}

func validDigest(value string) bool {
	return len(value) == len("sha256:")+64 && strings.HasPrefix(value, "sha256:") && validHex(strings.TrimPrefix(value, "sha256:"))
}

func validHex(value string) bool {
	if value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validSnapshotID(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
