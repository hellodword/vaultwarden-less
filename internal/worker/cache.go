package worker

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hellodword/vaultwarden-less/internal/capture"
	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/restic"
)

func nativeID(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func payloadName(value string) bool {
	if !strings.HasPrefix(value, "payload-") {
		return false
	}
	suffix := strings.TrimPrefix(value, "payload-")
	if suffix == "" {
		return false
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
func loadBaseline(ctx context.Context, cache string, target config.Target, latest *restic.Snapshot) (*capture.Payload, error) {
	if latest == nil || !nativeID(latest.ID) {
		return nil, nil
	}
	link, err := os.Readlink(filepath.Join(cache, "baselines", restic.CacheKey(target), latest.ID))
	if err != nil {
		return nil, err
	}
	name := filepath.Base(link)
	if link != filepath.Join("..", "..", "payloads", name) || !payloadName(name) {
		return nil, errors.New("invalid baseline reference")
	}
	tag := ""
	if slices.Contains(latest.Tags, "complete") {
		tag = "complete"
	}
	if slices.Contains(latest.Tags, "incomplete") {
		tag = "incomplete"
	}
	return capture.Load(ctx, filepath.Join(cache, "payloads", name), tag)
}
func saveBaseline(cache string, target config.Target, id string, p *capture.Payload) error {
	if !nativeID(id) || !payloadName(filepath.Base(p.Directory)) {
		return errors.New("invalid baseline association")
	}
	dir := filepath.Join(cache, "baselines", restic.CacheKey(target))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	link := filepath.Join(dir, id)
	if err := os.Symlink(filepath.Join("..", "..", "payloads", filepath.Base(p.Directory)), link); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != id && nativeID(entry.Name()) && entry.Type()&os.ModeSymlink != 0 {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
