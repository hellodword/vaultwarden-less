package capture

import (
	"fmt"
	"sort"
	"strings"
)

// These are value exclusions, never a schema or backup projection. Every
// unlisted table, column, JSON member and authentication type is retained.
var excludedColumns = map[string]string{
	"devices":              "uuid created_at updated_at user_uuid name atype push_token refresh_token twofactor_remember push_uuid",
	"auth_requests":        "uuid user_uuid organization_uuid request_device_identifier device_type request_ip response_device_id access_code public_key enc_key master_password_hash approved creation_date response_date authentication_date",
	"sso_auth":             "state client_challenge nonce redirect_uri code_response created_at updated_at binding_hash",
	"twofactor_incomplete": "user_uuid device_uuid device_name login_time ip_address device_type",
	"twofactor_duo_ctx":    "state user_email nonce exp",
	"event":                "uuid event_type user_uuid org_uuid cipher_uuid collection_uuid group_uuid org_user_uuid act_user_uuid device_type ip_address event_date policy_uuid provider_uuid provider_user_uuid provider_org_uuid",
	"users":                "updated_at last_verifying_at login_verify_count security_stamp email_new email_new_token",
	"sends":                "access_count revision_date",
	"emergency_access":     "last_notification_at updated_at",
}

var transientTables = map[string]bool{
	"devices": true, "auth_requests": true, "sso_auth": true,
	"twofactor_incomplete": true, "twofactor_duo_ctx": true, "event": true,
}

const temporaryFactorColumns = "uuid user_uuid atype enabled last_used"

type jsonRule struct {
	kind  string
	keys  map[string]*jsonRule
	item  *jsonRule
	units []string
}

func object(keys map[string]*jsonRule) *jsonRule { return &jsonRule{kind: "object", keys: keys} }
func array(item *jsonRule) *jsonRule             { return &jsonRule{kind: "array", item: item} }
func scalar() *jsonRule                          { return &jsonRule{kind: "scalar"} }
func scalars(names string) map[string]*jsonRule {
	keys := map[string]*jsonRule{}
	for _, name := range strings.Fields(names) {
		keys[name] = scalar()
	}
	return keys
}

func stampRule() *jsonRule {
	keys := scalars("security_stamp expire")
	keys["routes"] = array(&jsonRule{kind: "text"})
	return object(keys)
}

func ssoRule(column string) *jsonRule {
	if column == "code_response_error" {
		return object(scalars("error error_description"))
	}
	keys := scalars("refresh_token access_token identifier email email_verified user_name")
	keys["expires_in"] = object(scalars("secs nanos"))
	return object(keys)
}

func factorRule(kind int) *jsonRule {
	switch kind {
	case 1:
		return object(scalars("last_token token_sent attempts"))
	case 7:
		return array(object(map[string]*jsonRule{"credential": object(map[string]*jsonRule{
			"cred": object(scalars("counter backup_state backup_eligible")),
		})}))
	case 1002:
		return object(scalars("email last_token token_sent attempts"))
	case 2000:
		return object(scalars("token token_sent attempts"))
	case 1003:
		keys := scalars("policy challenge require_resident_key authenticator_attachment allow_synchronised_authenticators")
		keys["exclude_credentials"] = array(scalar())
		keys["credential_algorithms"] = array(scalar())
		keys["extensions"] = object(nil)
		return object(map[string]*jsonRule{"rs": object(keys)})
	case 1004:
		keys := scalars("policy challenge appid allow_backup_eligible_upgrade")
		keys["credentials"] = array(challengeCredentialRule())
		return object(map[string]*jsonRule{"ast": object(keys)})
	}
	return nil
}

func challengeCredentialRule() *jsonRule {
	keys := scalars("cred_id counter user_verified backup_eligible backup_state registration_policy attestation_format")
	keys["transports"] = array(scalar())
	rsa := scalars("n")
	rsa["e"] = array(scalar())
	keys["cred"] = object(map[string]*jsonRule{
		"type_": scalar(), "key": object(map[string]*jsonRule{
			"EC_OKP": object(scalars("curve x")), "EC_EC2": object(scalars("curve x y")), "RSA": object(rsa),
		}),
	})
	data := object(map[string]*jsonRule{"Basic": array(scalar()), "AttCa": array(scalar()), "AnonCa": array(scalar())})
	data.units = []string{"Self_", "ECDAA", "None", "Uncertain"}
	safety := scalars("apk_package_name cts_profile_match basic_integrity evaluation_type")
	safety["apk_certificate_digest_sha256"] = array(scalar())
	metadata := object(map[string]*jsonRule{
		"Packed": object(scalars("aaguid")), "Tpm": object(scalars("aaguid firmware_version")),
		"AndroidKey": object(scalars("is_km_tee is_attest_tee")), "AndroidSafetyNet": object(safety),
	})
	metadata.units = []string{"None"}
	keys["attestation"] = object(map[string]*jsonRule{"data": data, "metadata": metadata})
	extensions := map[string]*jsonRule{}
	for _, name := range []string{"cred_protect", "hmac_create_secret", "appid", "cred_props"} {
		value := scalar()
		if name == "cred_props" {
			value = object(scalars("rk"))
		}
		rule := object(map[string]*jsonRule{"Set": value, "Unsolicited": value, "Unsigned": value})
		rule.units = []string{"NotRequested", "Ignored"}
		extensions[name] = rule
	}
	keys["extensions"] = object(extensions)
	return object(keys)
}

// filteredJSON delegates all value handling to SQLite. json_tree supplies a
// typed, path-qualified multiset; only nodes of the explicitly known shape are
// omitted. Unknown empty objects and unexpected descendants remain nodes. This
// avoids removing an entire parent when a future upstream version extends it.
// SQL NULL means that no meaningful JSON content remains.
func filteredJSON(column string, root *jsonRule) string {
	var rules, edges []string
	var visit func(*jsonRule) int
	visit = func(rule *jsonRule) int {
		id := len(rules) + 1
		units := "'[]'"
		if len(rule.units) > 0 {
			values := make([]string, len(rule.units))
			for i, v := range rule.units {
				values[i] = literal(v)
			}
			units = "json_array(" + strings.Join(values, ",") + ")"
		}
		rules = append(rules, fmt.Sprintf("(%d,%s,%s)", id, literal(rule.kind), units))
		names := make([]string, 0, len(rule.keys))
		for name := range rule.keys {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			child := visit(rule.keys[name])
			edges = append(edges, fmt.Sprintf("(%d,%s,%d)", id, literal(name), child))
		}
		if rule.item != nil {
			child := visit(rule.item)
			edges = append(edges, fmt.Sprintf("(%d,NULL,%d)", id, child))
		}
		return id
	}
	visit(root)
	if len(edges) == 0 {
		edges = append(edges, "(0,NULL,0)")
	}
	return `CASE WHEN typeof(` + column + `)='text' AND json_valid(` + column + `) THEN (
	 WITH RECURSIVE
	 rules(rule,kind,units) AS (VALUES ` + strings.Join(rules, ",") + `),
	 edges(parent,key,child) AS (VALUES ` + strings.Join(edges, ",") + `),
	 tree AS MATERIALIZED (SELECT * FROM json_tree(` + column + `)),
	 nodes(id,fullkey,type,atom,rule) AS (
	   SELECT id,fullkey,type,atom,1 FROM tree WHERE parent IS NULL
	   UNION ALL
	   SELECT t.id,t.fullkey,t.type,t.atom,coalesce(e.child,0)
	   FROM nodes n JOIN tree t ON t.parent=n.id
	   LEFT JOIN rules r ON r.rule=n.rule AND r.kind=n.type
	   LEFT JOIN edges e ON e.parent=r.rule AND
	     ((r.kind='object' AND e.key=t.key) OR (r.kind='array' AND e.key IS NULL))
	 ),
	 remaining AS (
	   SELECT n.* FROM nodes n LEFT JOIN rules r ON r.rule=n.rule
	   WHERE NOT coalesce(
	     (r.kind IN ('object','array','text') AND r.kind=n.type) OR
	     (r.kind='scalar' AND n.type NOT IN ('object','array')) OR
	     (n.type='text' AND n.atom IN (SELECT value FROM json_each(r.units))),0)
	 )
	 SELECT CASE WHEN count(*)=0 THEN NULL ELSE
	   json_group_array(json_array(fullkey,type,atom) ORDER BY fullkey,id) END FROM remaining
	) ELSE ` + column + ` END`
}
