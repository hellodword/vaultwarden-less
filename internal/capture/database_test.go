package capture

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func sqlExec(t *testing.T, path, sql string) string {
	t.Helper()
	out, err := exec.Command("sqlite3", "-batch", "-bail", path, sql).CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite fixture: %v: %s", err, out)
	}
	return string(out)
}

func TestReadOnlyWALBackup(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	destination := filepath.Join(dir, "copy")
	// Keep a native SQLite writer open so committed content stays in WAL.
	cmd := exec.Command("sqlite3", "-batch", source)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { in.Close(); cmd.Wait() }()
	fmt.Fprintln(in, "PRAGMA journal_mode=WAL; CREATE TABLE t(v); INSERT INTO t VALUES('committed'); SELECT 'ready';")
	buf := make([]byte, 128)
	var response string
	for !strings.Contains(response, "ready") {
		n, e := out.Read(buf)
		if e != nil {
			t.Fatal(e)
		}
		response += string(buf[:n])
	}
	before := map[string][]byte{}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(source + suffix)
		if err != nil {
			t.Fatal(err)
		}
		before[suffix] = data
		if err := os.Chmod(source+suffix, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	if err := OnlineBackup(context.Background(), source, destination); err != nil {
		t.Fatal(err)
	}
	for suffix, want := range before {
		got, err := os.ReadFile(source + suffix)
		if err != nil || string(got) != string(want) {
			t.Fatalf("source%s changed", suffix)
		}
	}
	if got := sqlExec(t, destination, "SELECT v FROM t;"); got != "committed\n" {
		t.Fatal("committed WAL absent")
	}
	if err := OnlineBackup(context.Background(), source, destination); err == nil {
		t.Fatal("existing destination overwritten")
	}
}
