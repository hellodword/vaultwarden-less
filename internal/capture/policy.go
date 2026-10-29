package capture

// These few projections affect comparison only. Every snapshot retains the
// complete native database, including temporary authentication state.
var excludedColumns = map[string]string{
	"devices":          "updated_at",
	"users":            "updated_at last_verifying_at login_verify_count email_new email_new_token stamp_exception",
	"sends":            "revision_date",
	"emergency_access": "last_notification_at updated_at",
}

var transientTables = map[string]bool{
	"auth_requests": true, "sso_auth": true, "twofactor_incomplete": true,
	"twofactor_duo_ctx": true, "event": true,
}

// The only JSON projection removes three explicitly temporary email fields.
// Unexpected shapes stay intact; invalid JSON makes comparison fall back to
// copied database bytes. Unknown members, including empty containers, remain.
func emailFactorData(name string) string {
	return `CASE WHEN typeof(` + name + `)='text' AND json_type(` + name + `)='object' THEN json_remove(` + name + `,'$.last_token','$.token_sent','$.attempts') ELSE ` + name + ` END`
}
