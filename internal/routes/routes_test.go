package routes

import (
	"strings"
	"testing"
)

func TestExplicitExclusions(t *testing.T) {
	for _, line := range exclusions {
		method, pattern, _ := strings.Cut(line, " ")
		parts := strings.Split(pattern, "/")
		for i, part := range parts {
			if strings.HasPrefix(part, "{") {
				parts[i] = "value"
			}
		}
		path := strings.Join(parts, "/")
		if Trigger(method, path) {
			t.Errorf("explicit exclusion triggered: %s", line)
		}
		// An adjacent path can independently be another explicit exclusion
		// (e.g. /api/ciphers and /api/ciphers/{cipher_id}). This pattern itself
		// must never absorb the extra segment.
		if !Trigger("PATCH", path) || matches(pattern, path+"/extra") {
			t.Errorf("exclusion broadened: %s", line)
		}
		if strings.Contains(pattern, "{") {
			for i, part := range strings.Split(pattern, "/") {
				if strings.HasPrefix(part, "{") {
					parts[i] = ""
					break
				}
			}
			if !Trigger(method, strings.Join(parts, "/")) {
				t.Errorf("empty parameter excluded: %s", line)
			}
		}
	}
	for _, line := range []string{"GET /unknown", "HEAD /api/sync", "GET /API/sync", "GET /api//sync", "GET /api/../api/sync", "GET /api/%73ync", "POST /identity/connect/token", "POST /api/accounts/verify-password", "POST /api/accounts/security-stamp", "POST /api/accounts/email-token"} {
		m, p, _ := strings.Cut(line, " ")
		if !Trigger(m, p) {
			t.Errorf("request should trigger: %s", line)
		}
	}
}
func TestCatalogOnlyClassifiesAndBoundsUnknownInput(t *testing.T) {
	if route, _ := Classify("POST", "/api/ciphers/secret"); route != "POST /api/ciphers/{cipher_id}" {
		t.Fatalf("known route=%s", route)
	}
	if route, _ := Classify("GET", "/api/ciphers/organization-details"); route != "GET /api/ciphers/organization-details" {
		t.Fatalf("static route lost priority=%s", route)
	}
	s := Summary{}
	for i := 0; i < 10000; i++ {
		s.Record(strings.Repeat("X", i%100+1), "/secret/unknown?data", true)
	}
	if len(s.Unknown) != 1 || s.Unknown["OTHER"].Started != 10000 || len(s.Routes) != 0 {
		t.Fatal("unknown input grew catalog")
	}
	if !s.Valid() {
		t.Fatal("valid summary rejected")
	}
	s.Routes["GET /raw-secret"] = Counts{Started: 1}
	if s.Valid() {
		t.Fatal("raw external route accepted")
	}
}
