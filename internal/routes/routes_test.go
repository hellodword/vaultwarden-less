package routes

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeployedRules(t *testing.T) {
	rules, err := Load("../../rules/routes.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method, path string
		trigger      bool
	}{
		{"GET", "/api/sync", false}, {"GET", "/api/ciphers/item", false}, {"GET", "/api/ciphers/a/b", true},
		{"GET", "/api/ciphers/%2F", true}, {"GET", "/api/ciphers/..", true}, {"POST", "/api/ciphers/item", true},
		{"POST", "/identity/connect/token", true}, {"GET", "/new-route", true},
	} {
		if rules.Trigger(c.method, c.path) != c.trigger {
			t.Errorf("wrong policy for %s %s", c.method, c.path)
		}
	}
	path := filepath.Join(t.TempDir(), "routes.txt")
	os.WriteFile(path, []byte("GET /test/{broken\n"), 0600)
	if _, err := Load(path); err == nil {
		t.Fatal("malformed rule enabled")
	}
}
