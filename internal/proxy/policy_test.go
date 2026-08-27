package proxy

import (
	"net/http"
	"strings"
	"testing"
)

func TestRoutePolicyVersionAndAuditedIgnoreTable(t *testing.T) {
	t.Parallel()
	seen := map[string]struct{}{}
	for _, rule := range ignoredRoutes {
		if rule.source == "" || rule.reason == "" {
			t.Fatalf("route lacks audit evidence: %#v", rule)
		}
		key := rule.method + " " + rule.pattern
		if _, duplicate := seen[key]; duplicate {
			t.Fatalf("duplicate route %s", key)
		}
		seen[key] = struct{}{}
		path := concretePath(rule.pattern)
		if shouldRequestBackup(rule.method, path, http.StatusNoContent) {
			t.Errorf("ignored route triggered: %s", key)
		}
		if !shouldRequestBackup(http.MethodPatch, path, http.StatusNoContent) {
			t.Errorf("wrong method was also ignored: PATCH %s", path)
		}
		if !shouldRequestBackup(rule.method, path+"/adjacent/extra", http.StatusNoContent) {
			t.Errorf("adjacent path was also ignored: %s %s", rule.method, path)
		}
		if strings.Contains(rule.pattern, "{") {
			emptySegment := rule.pattern
			start := strings.IndexByte(emptySegment, '{')
			end := strings.IndexByte(emptySegment[start:], '}') + start
			emptySegment = emptySegment[:start] + emptySegment[end+1:]
			if !shouldRequestBackup(rule.method, emptySegment, http.StatusNoContent) {
				t.Errorf("empty parameter segment was ignored: %s %s", rule.method, emptySegment)
			}
		}
	}
}

func TestRoutePolicyOnlySuccessfulWritesTrigger(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		method string
		path   string
		status int
		want   bool
	}{
		{http.MethodPost, "/api/ciphers", 200, true},
		{http.MethodPut, "/api/folders/folder", 201, true},
		{http.MethodPatch, "/api/ciphers/item", 204, true},
		{http.MethodDelete, "/api/sends/send", 299, true},
		{http.MethodPost, "/api/sends/access", 200, true},
		{http.MethodPost, "/api/sends/access/value", 200, true},
		{http.MethodPost, "/api/sends/access/file/value", 200, true},
		{http.MethodPost, "/api/sends/value/access/file/value", 200, true},
		{http.MethodGet, "/api/ciphers", 200, false},
		{http.MethodHead, "/api/ciphers", 200, false},
		{http.MethodOptions, "/api/ciphers", 200, false},
		{http.MethodTrace, "/api/ciphers", 200, false},
		{http.MethodPost, "/api/ciphers", 199, false},
		{http.MethodPost, "/api/ciphers", 300, false},
		{http.MethodPost, "/api/ciphers", 404, false},
		{http.MethodPost, "/api/ciphers", 500, false},
		{http.MethodPost, "/api/accounts/prelogin", 200, false},
		{http.MethodPost, "/api/uncommon-read-like-operation", 200, true},
	} {
		if got := shouldRequestBackup(test.method, test.path, test.status); got != test.want {
			t.Errorf("%s %s/%d = %t, want %t", test.method, test.path, test.status, got, test.want)
		}
	}
}

func concretePath(pattern string) string {
	segments := strings.Split(pattern, "/")
	for index, segment := range segments {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			segments[index] = "value"
		}
	}
	return strings.Join(segments, "/")
}
