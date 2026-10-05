package e2e_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const stubSecret = "e2e-notification-signing-secret"
const stubDevice = "synthetic-device-token-with-no-fixed-length"
const notificationConfiguration = `notifiers:
  - name: wecom
    type: wecom
    webhook_url: https://api.push.apple.com/wecom
  - name: feishu
    type: feishu
    webhook_url: https://api.push.apple.com/feishu
    secret: e2e-notification-signing-secret
  - name: dingtalk
    type: dingtalk
    webhook_url: https://api.push.apple.com/dingtalk
    secret: e2e-notification-signing-secret
  - name: bark
    type: bark
    device_token: synthetic-device-token-with-no-fixed-length
    auth_key_file: /stub/auth.p8
    key_id: TESTKEY123
    team_id: TESTTEAM12
    topic: me.fin.bark
`

type stubChannel struct {
	Attempts       int
	Successes      int
	Failures       int
	Invalid        int
	HTTP2          int
	Markdown       int
	FirstAttempt   time.Time
	FirstSuccess   time.Time
	UnknownSummary bool
}

// This test-only process is packaged inside the runner. Docker DNS supplies
// Apple's production hostname, and the backup process trusts only the mounted
// test CA for this peer. No production endpoint override is involved.
func TestMain(m *testing.M) {
	if os.Getenv("VWLESS_E2E_REJECTOR") == "1" {
		server := &http.Server{Addr: ":5000", ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })}
		if err := server.ListenAndServe(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if root := os.Getenv("VWLESS_E2E_STUB"); root != "" {
		if err := runNotificationStub(root); err != nil {
			fmt.Fprintln(os.Stderr, "notification stub failed")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func (h *e2eHarness) prepareNotificationStub() {
	h.t.Helper()
	root := filepath.Join(h.testRoot, "stub")
	if err := os.Mkdir(root, 0700); err != nil {
		h.t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		h.t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "acceptance CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"api.push.apple.com"}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		h.t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	writePrivateFile(h.t, filepath.Join(root, "ca.pem"), string(encoded))
	p8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		h.t.Fatal(err)
	}
	writePrivateFile(h.t, filepath.Join(root, "server.pem"), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p8})))
	auth, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		h.t.Fatal(err)
	}
	p8, err = x509.MarshalPKCS8PrivateKey(auth)
	if err != nil {
		h.t.Fatal(err)
	}
	writePrivateFile(h.t, filepath.Join(root, "auth.p8"), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p8})))
	writePrivateFile(h.t, filepath.Join(root, "stats.json"), "{}")
}

func runNotificationStub(root string) error {
	data, err := os.ReadFile(filepath.Join(root, "auth.p8"))
	if err != nil {
		return err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return fmt.Errorf("auth key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return err
	}
	auth, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return fmt.Errorf("auth type")
	}
	var mu sync.Mutex
	stats := map[string]stubChannel{}
	server := &http.Server{Addr: ":443", ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		channel := strings.TrimPrefix(r.URL.Path, "/")
		if r.URL.Path == "/3/device/"+stubDevice {
			channel = "bark"
		}
		valid := r.Method == "POST"
		raw, err := io.ReadAll(io.LimitReader(r.Body, 20001))
		if err != nil {
			valid = false
		}
		var doc map[string]any
		if json.Unmarshal(raw, &doc) != nil {
			valid = false
		}
		content := ""
		switch channel {
		case "wecom", "dingtalk":
			markdown, _ := doc["markdown"].(map[string]any)
			valid = valid && doc["msgtype"] == "markdown" && len(raw) <= 20000
			if channel == "wecom" {
				content, _ = markdown["content"].(string)
				valid = valid && len(content) <= 4096
			}
			if channel == "dingtalk" {
				content, _ = markdown["text"].(string)
				title, _ := markdown["title"].(string)
				valid = valid && strings.HasPrefix(title, "vaultwarden-less · ") && len(content) <= 4000
				ts := r.URL.Query().Get("timestamp")
				when, err := strconv.ParseInt(ts, 10, 64)
				valid = valid && err == nil && time.Since(time.UnixMilli(when)).Abs() < time.Minute && hmac.Equal([]byte(r.URL.Query().Get("sign")), []byte(stubHMAC(stubSecret, ts+"\n"+stubSecret)))
			}
		case "feishu":
			card, _ := doc["card"].(map[string]any)
			header, _ := card["header"].(map[string]any)
			title, _ := header["title"].(map[string]any)
			body, _ := card["body"].(map[string]any)
			elements, _ := body["elements"].([]any)
			valid = valid && doc["msg_type"] == "interactive" && card["schema"] == "2.0" && title["tag"] == "plain_text" && len(elements) == 1
			if len(elements) == 1 {
				element, _ := elements[0].(map[string]any)
				content, _ = element["content"].(string)
				valid = valid && element["tag"] == "markdown"
			}
			ts, _ := doc["timestamp"].(string)
			sig, _ := doc["sign"].(string)
			when, err := strconv.ParseInt(ts, 10, 64)
			valid = valid && len(raw) <= 20000 && err == nil && time.Since(time.Unix(when, 0)).Abs() < time.Minute && hmac.Equal([]byte(sig), []byte(stubHMAC(ts+"\n"+stubSecret, "")))
		case "bark":
			aps, _ := doc["aps"].(map[string]any)
			alert, _ := aps["alert"].(map[string]any)
			content, _ = doc["markdown"].(string)
			fallback, _ := alert["body"].(string)
			title, _ := alert["title"].(string)
			valid = valid && strings.HasPrefix(title, "vaultwarden-less · ") && strings.HasPrefix(fallback, "Source: ") && !strings.Contains(fallback, "**")
			valid = valid && r.ProtoMajor == 2 && r.Header.Get("apns-topic") == "me.fin.bark" && r.Header.Get("apns-push-type") == "alert" && len(raw) <= 4096
			valid = valid && aps["sound"] == "1107" && aps["category"] == "myNotificationCategory" && aps["mutable-content"] == float64(1) && aps["thread-id"] == "vaultwarden-less" && doc["isarchive"] == "1" && doc["group"] == "vaultwarden-less" && doc["badge"] == "1"
			valid = valid && validStubJWT(r.Header.Get("authorization"), &auth.PublicKey)
		default:
			valid = false
		}
		valid = valid && strings.HasPrefix(content, "**vaultwarden-less · ") && strings.Contains(content, "**Source:** ") && strings.Contains(content, "**Summary:** ") && !strings.Contains(content, "e2e-private-path")
		mu.Lock()
		defer mu.Unlock()
		state := stats[channel]
		state.Attempts++
		if state.FirstAttempt.IsZero() {
			state.FirstAttempt = time.Now()
		}
		if r.ProtoMajor == 2 {
			state.HTTP2++
		}
		if valid {
			state.Markdown++
			for _, paragraph := range strings.Split(content, "\n\n") {
				if paragraph == "> Unknown GET: started 8, finished 8" {
					state.UnknownSummary = true
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if !valid {
			state.Invalid++
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"reason":"BadPayload"}`))
		} else if state.Failures == 0 {
			state.Failures++
			if channel == "bark" {
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(429)
				_, _ = w.Write([]byte(`{"reason":"TooManyRequests"}`))
			} else if channel == "feishu" {
				_, _ = w.Write([]byte(`{"code":19001}`))
			} else {
				_, _ = w.Write([]byte(`{"errcode":93000}`))
			}
		} else {
			state.Successes++
			if state.FirstSuccess.IsZero() {
				state.FirstSuccess = time.Now()
			}
			if channel == "feishu" {
				_, _ = w.Write([]byte(`{"code":0}`))
			} else if channel != "bark" {
				_, _ = w.Write([]byte(`{"errcode":0}`))
			}
		}
		stats[channel] = state
		encoded, _ := json.MarshalIndent(stats, "", "  ")
		if os.WriteFile(filepath.Join(root, "stats.new"), encoded, 0600) == nil {
			_ = os.Rename(filepath.Join(root, "stats.new"), filepath.Join(root, "stats.json"))
		}
	})}
	return server.ListenAndServeTLS(filepath.Join(root, "ca.pem"), filepath.Join(root, "server.pem"))
}
func stubHMAC(key, message string) string {
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(message))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
func validStubJWT(header string, key *ecdsa.PublicKey) bool {
	if !strings.HasPrefix(header, "bearer ") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(header, "bearer "), ".")
	if len(parts) != 3 {
		return false
	}
	var head struct{ Alg, Kid string }
	var claims struct {
		Iss string
		Iat int64
	}
	a, e1 := base64.RawURLEncoding.DecodeString(parts[0])
	b, e2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, e3 := base64.RawURLEncoding.DecodeString(parts[2])
	if e1 != nil || e2 != nil || e3 != nil || json.Unmarshal(a, &head) != nil || json.Unmarshal(b, &claims) != nil || len(sig) != 64 {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return head.Alg == "ES256" && head.Kid == "TESTKEY123" && claims.Iss == "TESTTEAM12" && time.Since(time.Unix(claims.Iat, 0)) >= -time.Minute && time.Since(time.Unix(claims.Iat, 0)) < time.Hour && ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}
func (h *e2eHarness) assertNotifications() {
	h.wait(3*time.Minute, "all four notification channels recover after an explicit failure", func() bool {
		var stats map[string]stubChannel
		data, err := os.ReadFile(filepath.Join(h.testRoot, "stub/stats.json"))
		if err != nil || json.Unmarshal(data, &stats) != nil {
			return false
		}
		for _, name := range []string{"wecom", "feishu", "dingtalk", "bark"} {
			state := stats[name]
			if state.Invalid > 0 || state.Markdown != state.Attempts {
				h.t.Fatalf("notification %s failed protocol verification", name)
			}
			if state.Successes == 0 || state.Failures != 1 || !state.UnknownSummary {
				return false
			}
			if state.FirstSuccess.Sub(state.FirstAttempt) < 59*time.Second {
				h.t.Fatalf("notification %s retried too soon", name)
			}
			if name == "bark" && state.HTTP2 != state.Attempts {
				h.t.Fatal("APNs transport was not HTTP/2")
			}
		}
		return true
	})
}
