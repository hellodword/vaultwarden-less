package capture

import (
	"path/filepath"
	"testing"
)

func TestPersonalVaultRecoveryBoundary(t *testing.T) {
	b := Builder{DataDir: t.TempDir(), CacheDir: t.TempDir()}
	var err error
	b.Rule, err = LoadRule("../../rules/personal-vault.sql")
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(b.DataDir, "db.sqlite3")
	sqlExec(t, db, `CREATE TABLE users(uuid,email,akey,key_id,private_key,public_key,client_kdf_type,client_kdf_iter,client_kdf_memory,client_kdf_parallelism,password_hash,security_stamp);
    CREATE TABLE ciphers(uuid,user_uuid,organization_uuid,created_at,key,atype,name,notes,fields,data,password_history,deleted_at,reprompt,updated_at);
    CREATE TABLE folders(uuid,user_uuid,name); CREATE TABLE folders_ciphers(folder_uuid,cipher_uuid);
    CREATE TABLE favorites(user_uuid,cipher_uuid); CREATE TABLE archives(user_uuid,cipher_uuid,archived_at);
    CREATE TABLE devices(id); CREATE TABLE twofactor(id);
    INSERT INTO users(uuid,akey) VALUES ('u','key');
    INSERT INTO ciphers(uuid,user_uuid,data) VALUES ('i','u','secret');
    INSERT INTO ciphers(uuid,organization_uuid,data) VALUES ('o','org','shared');
    INSERT INTO folders VALUES('f','u','Work');`)
	previous, err := b.Build(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		sql  string
		want bool
	}{
		{"INSERT INTO devices VALUES(1); INSERT INTO twofactor VALUES(2); UPDATE users SET password_hash='login',security_stamp=1; UPDATE ciphers SET updated_at=1;", false},
		{"UPDATE ciphers SET data='org change' WHERE uuid='o'; INSERT INTO favorites VALUES('u','o'); INSERT INTO folders_ciphers VALUES ('f','o');", false},
		{"UPDATE ciphers SET data='edited' WHERE uuid='i';", true},
		{"INSERT INTO folders_ciphers VALUES('f','i');", true}, {"UPDATE folders SET name='Home';", true},
		{"INSERT INTO favorites VALUES('u','i');", true}, {"DELETE FROM favorites WHERE cipher_uuid='i';", true},
		{"INSERT INTO archives VALUES('u','i',1);", true}, {"UPDATE ciphers SET deleted_at=1 WHERE uuid='i';", true},
		{"UPDATE users SET akey='rotated',client_kdf_iter=34;", true}, {"DELETE FROM ciphers WHERE uuid='i';", true},
	} {
		sqlExec(t, db, change.sql)
		current, err := b.Build(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, pair := range [][2]string{{previous, current}, {current, previous}} {
			got, err := b.Rule.Changed(t.Context(), pair[0], pair[1])
			if err != nil || got != change.want {
				t.Fatalf("%s: changed=%v, err=%v", change.sql, got, err)
			}
		}
		previous = current
	}
	sqlExec(t, db, "ALTER TABLE users RENAME COLUMN akey TO lost;")
	if _, err = b.Build(t.Context()); err == nil {
		t.Fatal("unknown key schema accepted for first snapshot")
	}
	sqlExec(t, db, "ALTER TABLE users RENAME COLUMN lost TO akey; CREATE TABLE bad(v CHECK(v>0)); PRAGMA ignore_check_constraints=ON; INSERT INTO bad VALUES(-1);")
	if _, err = b.Build(t.Context()); err == nil {
		t.Fatal("damaged database accepted")
	}
}
