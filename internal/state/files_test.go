package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicRecordsAndSingleInstance(t *testing.T) {
	s := Store{Directory: t.TempDir()}
	lock, err := s.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if second, err := s.Lock(); err == nil {
		second.Close()
		t.Fatal("second worker acquired state lock")
	}
	if err := s.Save("record.json", map[string]int{"saved": 3}); err != nil {
		t.Fatal(err)
	}
	var value map[string]int
	if err := s.Load("record.json", &value); err != nil || value["saved"] != 3 {
		t.Fatal("record not durable")
	}
	lock.Close()
	second, err := s.Lock()
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
	if err := os.WriteFile(filepath.Join(s.Directory, "record.json"), []byte("truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Load("record.json", &value); err == nil {
		t.Fatal("corrupt state silently reset")
	}
}
