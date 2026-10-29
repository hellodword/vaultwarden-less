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

func digests(t *testing.T, path string) Digest {
	t.Helper()
	d, err := CompareDigest(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestTypedComparisonAndSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	sqlExec(t, path, `CREATE TABLE records(v); INSERT INTO records VALUES(NULL),(1),(1.0),(x'31'),('1'),('same'),('same');`)
	before := digests(t, path)
	sqlExec(t, path, `VACUUM; REINDEX;`)
	if got := digests(t, path); got != before {
		t.Fatal("physical rewrite changed logical digest")
	}
	for _, sql := range []string{
		`DELETE FROM records WHERE rowid=(SELECT min(rowid) FROM records WHERE v='same');`,
		`UPDATE records SET v=1.0 WHERE typeof(v)='integer';`,
		`UPDATE records SET v='1' WHERE typeof(v)='blob';`,
		`CREATE TABLE empty (id);`, `ALTER TABLE empty ADD COLUMN future TEXT;`,
		`CREATE VIEW future_view AS SELECT * FROM records;`,
		`CREATE INDEX future_index ON records(v);`,
		`CREATE TRIGGER future_trigger AFTER INSERT ON empty BEGIN SELECT 1; END;`,
		`PRAGMA user_version=9;`, `PRAGMA application_id=42;`,
	} {
		sqlExec(t, path, sql)
		after := digests(t, path)
		if after == before {
			t.Fatalf("missed %s", sql)
		}
		before = after
	}
}

// Table-driven transitions cover the explicit policy and conservative defaults.
func TestEffectiveContentProjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	sqlExec(t, path, `CREATE TABLE users(uuid PRIMARY KEY,password_hash,updated_at,security_stamp,stamp_exception);
 INSERT INTO users VALUES('u','hash','old','stamp',NULL);
 CREATE TABLE devices(uuid PRIMARY KEY,updated_at,refresh_token,twofactor_remember,future);
 INSERT INTO devices VALUES('d','old','token','remember',NULL);
 CREATE TABLE twofactor(uuid PRIMARY KEY,user_uuid,atype INTEGER,enabled,last_used,data);
 INSERT INTO twofactor VALUES('totp','u',0,1,0,'secret'),('email','u',1,1,0,'{"email":"a","attempts":0}'),('key','u',7,1,0,'[{"counter":1,"backup_state":false}]');
 CREATE TABLE sends(uuid, max_access_count, access_count, revision_date); INSERT INTO sends VALUES('s',NULL,0,'old');
 CREATE TABLE emergency_access(status,updated_at,last_notification_at,recovery_initiated_at); INSERT INTO emergency_access VALUES(0,'old',NULL,NULL);
 CREATE TABLE auth_requests(data); CREATE TABLE sso_auth(data); CREATE TABLE twofactor_incomplete(data); CREATE TABLE twofactor_duo_ctx(data); CREATE TABLE event(data);`)
	cases := []struct {
		sql     string
		changed bool
	}{
		{`UPDATE users SET updated_at='new',stamp_exception='anything';`, false},
		{`UPDATE users SET security_stamp='revoked';`, true},
		{`UPDATE users SET password_hash='upgraded';`, true},
		{`UPDATE devices SET updated_at='new';`, false},
		{`UPDATE devices SET refresh_token='rotated';`, true},
		{`UPDATE devices SET twofactor_remember='revoked';`, true},
		{`UPDATE devices SET future='durable';`, true},
		{`DELETE FROM devices;`, true},
		{`UPDATE twofactor SET last_used=1 WHERE atype=0;`, false},
		{`UPDATE twofactor SET data='new-secret' WHERE atype=0;`, true},
		{`UPDATE twofactor SET data='{"email":"a","last_token":"temp","token_sent":42,"attempts":2}' WHERE atype=1;`, false},
		{`UPDATE twofactor SET data='{"email":"b","attempts":3}' WHERE atype=1;`, true},
		{`UPDATE twofactor SET data='{"email":"b","future":{}}' WHERE atype=1;`, true},
		{`UPDATE twofactor SET uuid='new-key' WHERE atype=7;`, false},
		{`UPDATE twofactor SET data='[{"counter":2,"backup_state":true}]' WHERE atype=7;`, true},
		{`INSERT INTO twofactor SELECT 'duplicate',user_uuid,atype,enabled,last_used,data FROM twofactor WHERE atype=7;`, true},
		{`UPDATE sends SET access_count=1,revision_date='new';`, false},
		{`UPDATE sends SET max_access_count=5;`, true},
		{`UPDATE sends SET access_count=2;`, true},
		{`UPDATE sends SET max_access_count=10;`, true},
		{`UPDATE sends SET max_access_count=NULL;`, true},
		{`UPDATE sends SET access_count=3;`, false},
		{`UPDATE emergency_access SET updated_at='new',last_notification_at='now';`, false},
		{`UPDATE emergency_access SET recovery_initiated_at='now';`, true},
		{`UPDATE emergency_access SET status=1;`, true},
		{`INSERT INTO twofactor VALUES('future','u',9001,1,0,'{}');`, true},
	}
	for _, table := range []string{"auth_requests", "sso_auth", "twofactor_incomplete", "twofactor_duo_ctx", "event"} {
		cases = append(cases, struct {
			sql     string
			changed bool
		}{"INSERT INTO " + table + " VALUES('temporary');", false})
	}
	for _, kind := range []int{1000, 1001, 1002, 1003, 1004, 2000} {
		cases = append(cases, struct {
			sql     string
			changed bool
		}{fmt.Sprintf("INSERT INTO twofactor VALUES('temp%d','u',%d,1,0,'{\"future\":{}}');", kind, kind), false})
	}
	for _, tc := range cases {
		before := digests(t, path)
		sqlExec(t, path, tc.sql)
		after := digests(t, path)
		if (before.Business != after.Business) != tc.changed {
			t.Fatalf("effective change=%v for %s", !tc.changed, tc.sql)
		}
		if before.Full == after.Full {
			t.Fatalf("full content not retained: %s", tc.sql)
		}
	}
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
