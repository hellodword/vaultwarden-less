package proxy

import (
	"net/http"
	"strings"
)

type routeRule struct {
	method  string
	pattern string
}

// ignoredRoutes is audited against the Vaultwarden revision in UPSTREAMS.toml.
// These routes change authentication, device, telemetry, or discovery state,
// not the durable user vault that a recovery point is intended to preserve.
var ignoredRoutes = []routeRule{
	// src/api/identity.rs:1026 — prelogin derives parameters without changing vault content.
	{http.MethodPost, "/identity/accounts/prelogin"},
	// src/api/identity.rs:1031 — password prelogin derives parameters without changing vault content.
	{http.MethodPost, "/identity/accounts/prelogin/password"},
	// src/api/core/accounts.rs:1319 — API prelogin is a read-like account lookup.
	{http.MethodPost, "/api/accounts/prelogin"},
	// src/api/core/two_factor/email.rs:39 — sending a login code is transient authentication activity.
	{http.MethodPost, "/api/two-factor/send-email-login"},
	// src/api/core/two_factor/protected_actions.rs:64 — requesting an OTP is transient authentication activity.
	{http.MethodPost, "/api/accounts/request-otp"},
	// src/api/core/two_factor/protected_actions.rs:106 — verifying an OTP is transient authentication activity.
	{http.MethodPost, "/api/accounts/verify-otp"},
	// src/api/core/accounts.rs:1487 — a push token is replaceable device delivery state.
	{http.MethodPost, "/api/devices/identifier/{device_id}/token"},
	// src/api/core/accounts.rs:1492 — a push token is replaceable device delivery state.
	{http.MethodPut, "/api/devices/identifier/{device_id}/token"},
	// src/api/core/accounts.rs:1543 — clearing a push token changes replaceable device delivery state.
	{http.MethodPost, "/api/devices/identifier/{device_id}/clear-token"},
	// src/api/core/accounts.rs:1519 — clearing a push token changes replaceable device delivery state.
	{http.MethodPut, "/api/devices/identifier/{device_id}/clear-token"},
	// src/api/core/accounts.rs:1568 — an auth request is short-lived login approval state.
	{http.MethodPost, "/api/auth-requests"},
	// src/api/core/accounts.rs:1656 — an auth response is short-lived login approval state.
	{http.MethodPut, "/api/auth-requests/{auth_request_id}"},
	// src/api/core/events.rs:164 — event collection is telemetry/audit ingestion, not vault content.
	{http.MethodPost, "/events/collect"},
	// src/api/core/organizations.rs:923 — SSO domain verification is a read-like discovery call.
	{http.MethodPost, "/api/organizations/domain/sso/verified"},
	// src/api/core/organizations.rs:1753 — public-key lookup is read-like member discovery.
	{http.MethodPost, "/api/organizations/{org_id}/users/public-keys"},
}

func shouldRequestBackup(method, externalPath string, status int) bool {
	if status < http.StatusOK || status >= http.StatusMultipleChoices || !isWriteMethod(method) {
		return false
	}
	for _, rule := range ignoredRoutes {
		if method == rule.method && matchesRoute(rule.pattern, externalPath) {
			return false
		}
	}
	return true
}

func isWriteMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return false
	default:
		return true
	}
}

func matchesRoute(pattern, requestPath string) bool {
	if pattern == "" || requestPath == "" || pattern[0] != '/' || requestPath[0] != '/' {
		return false
	}
	patternSegments := strings.Split(pattern[1:], "/")
	requestSegments := strings.Split(requestPath[1:], "/")
	if len(patternSegments) != len(requestSegments) {
		return false
	}
	for index, patternSegment := range patternSegments {
		requestSegment := requestSegments[index]
		parameter := len(patternSegment) >= 3 && patternSegment[0] == '{' && patternSegment[len(patternSegment)-1] == '}'
		if parameter {
			if requestSegment == "" {
				return false
			}
			continue
		}
		if requestSegment != patternSegment {
			return false
		}
	}
	return true
}
