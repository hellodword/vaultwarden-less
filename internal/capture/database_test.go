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

func TestOrdinaryExclusionsPreserveFullDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	sqlExec(t, path, `CREATE TABLE users(uuid TEXT PRIMARY KEY,password_hash,salt,password_iterations,updated_at,security_stamp,stamp_exception);
INSERT INTO users VALUES('u','hash','salt',1,'one','stamp',NULL);
CREATE TABLE devices(uuid TEXT PRIMARY KEY,name); INSERT INTO devices VALUES('d','a');
CREATE TABLE sso_auth(state TEXT PRIMARY KEY,auth_response,code_response_error);`)
	before := digests(t, path)
	sqlExec(t, path, `UPDATE users SET updated_at='two',security_stamp='other',stamp_exception='{"security_stamp":"a","expire":1,"routes":["/a","/b"]}';
DELETE FROM devices; INSERT INTO devices VALUES('new','b');
INSERT INTO sso_auth VALUES('s','{"email":"a","expires_in":{"secs":2,"nanos":0}}','{"error":"bad","error_description":"x"}');`)
	after := digests(t, path)
	if before.Business != after.Business || before.Full == after.Full {
		t.Fatal("authentication state was not excluded only from ordinary comparison")
	}
	if got := sqlExec(t, path, `SELECT password_hash||':'||security_stamp FROM users;`); got != "hash:other\n" {
		t.Fatal("comparison modified database")
	}
	sqlExec(t, path, `UPDATE users SET password_hash='upgraded';`)
	if next := digests(t, path); next.Business == after.Business {
		t.Fatal("password hash upgrade ignored")
	}
	sqlExec(t, path, `ALTER TABLE devices ADD COLUMN future;`)
	before = digests(t, path)
	sqlExec(t, path, `DELETE FROM devices; INSERT INTO devices VALUES('another','b',NULL);`)
	if digests(t, path).Business == before.Business {
		t.Fatal("new column lost primary key context")
	}
}

func TestJSONExclusionsAreStructural(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rule    *jsonRule
		known   string
		unknown []string
	}{
		{"stamp", stampRule(), `{"security_stamp":"x","expire":1,"routes":["a"]}`, []string{`{"routes":[{}]}`, `{"routes":{"future":{}}}`, `{"future":{}}`, `{"expire":{"future":null}}`}},
		{"sso", ssoRule("auth_response"), `{"access_token":"x","expires_in":{"secs":1,"nanos":2}}`, []string{`{"expires_in":{"future":{}}}`, `{"expires_in":[]}`, `{"future":[]}`}},
		{"register", factorRule(1003), `{"rs":{"policy":"x","challenge":"y","exclude_credentials":["a"],"credential_algorithms":[1],"extensions":{}}}`, []string{`{"rs":{"extensions":{"future":{}}}}`, `{"rs":{"credential_algorithms":[{}]}}`, `{"rs":{"future":{}}}`}},
		{"login", factorRule(1004), `{"ast":{"credentials":[{"cred_id":"x","transports":["usb"],"cred":{"type_":"EC2","key":{"EC_EC2":{"curve":1,"x":"x","y":"y"}}},"attestation":{"data":"None","metadata":"None"},"extensions":{"cred_protect":"Ignored","cred_props":{"Set":{"rk":true}}}}]}}`, []string{`{"ast":{"credentials":[{"cred":{"key":{"EC_EC2":{"future":{}}}}}]}}`, `{"ast":{"credentials":[{"extensions":{"cred_props":{"Set":{"future":{}}}}}]}}`, `{"ast":{"credentials":[{"attestation":{"data":"Future"}}]}}`, `{"ast":{"credentials":[{"attestation":{"data":{"Basic":[{}]}}}]}}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			sqlExec(t, path, "CREATE TABLE input(data); INSERT INTO input VALUES("+literal(tc.known)+");")
			expression := filteredJSON("data", tc.rule)
			if got := sqlExec(t, path, "SELECT ("+expression+") IS NULL FROM input;"); got != "1\n" {
				t.Fatalf("known content remained: %s", got)
			}
			for _, value := range tc.unknown {
				sqlExec(t, path, "UPDATE input SET data="+literal(value)+";")
				if got := sqlExec(t, path, "SELECT ("+expression+") IS NULL FROM input;"); got != "0\n" {
					t.Fatalf("unknown content disappeared: %s", value)
				}
			}
			for _, value := range []string{"bad json", `"unexpected scalar"`, `42`, `null`} {
				sqlExec(t, path, "UPDATE input SET data="+literal(value)+";")
				if got := sqlExec(t, path, "SELECT ("+expression+") IS NULL FROM input;"); got != "0\n" {
					t.Fatalf("unexpected value disappeared: %s", value)
				}
			}
		})
	}
}

func TestFactorTypesAndDuplicateRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	sqlExec(t, path, `CREATE TABLE twofactor(uuid TEXT PRIMARY KEY,user_uuid,atype INTEGER,enabled,last_used,data);
INSERT INTO twofactor VALUES('p','u',7,1,0,'[{"credential":{"cred":{"counter":1,"backup_state":false,"key":"persistent"}}}]');
INSERT INTO twofactor VALUES('e','u',1,1,0,'{"email":"a","attempts":0}');
INSERT INTO twofactor VALUES('o','u',0,1,0,'secret');`)
	before := digests(t, path)
	sqlExec(t, path, `UPDATE twofactor SET uuid='new-p',data='[{"credential":{"cred":{"counter":9,"backup_state":true,"key":"persistent"}}}]' WHERE atype=7;
UPDATE twofactor SET data='{"email":"a","attempts":8}' WHERE atype=1;
UPDATE twofactor SET last_used=5 WHERE atype=0;`)
	if got := digests(t, path); got.Business != before.Business || got.Full == before.Full {
		t.Fatal("persistent factor exclusions incorrect")
	}
	for _, kind := range []int{1002, 1003, 1004, 2000} {
		before = digests(t, path)
		sqlExec(t, path, fmt.Sprintf("INSERT INTO twofactor VALUES('temp%d','u',%d,1,3,'{}');", kind, kind))
		if digests(t, path).Business != before.Business {
			t.Fatalf("temporary factor %d row included", kind)
		}
		sqlExec(t, path, fmt.Sprintf("UPDATE twofactor SET data='{\"future\":{}}' WHERE atype=%d;", kind))
		if digests(t, path).Business == before.Business {
			t.Fatalf("temporary factor %d unknown key ignored", kind)
		}
	}
	before = digests(t, path)
	sqlExec(t, path, `INSERT INTO twofactor SELECT 'duplicate',user_uuid,atype,enabled,last_used,data FROM twofactor WHERE atype=7;`)
	if digests(t, path).Business == before.Business {
		t.Fatal("duplicate credential row was deduplicated")
	}
	before = digests(t, path)
	sqlExec(t, path, `INSERT INTO twofactor VALUES('future','u',9001,1,0,'{}');`)
	if digests(t, path).Business == before.Business {
		t.Fatal("unknown factor type ignored")
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
