package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// issue mints a token signed by a freshly-generated key and swaps that key in
// as the trusted one for the duration of the test.
func issue(t *testing.T, c Claims) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	orig := SigningPublicKey
	SigningPublicKey = pub
	t.Cleanup(func() { SigningPublicKey = orig })
	payload, _ := json.Marshal(c)
	sig := ed25519.Sign(priv, payload)
	return "NIMBO-LIC-1." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestVerifyValid(t *testing.T) {
	tok := issue(t, Claims{Customer: "Acme Ltd", Tier: TierBusiness, Seats: 50, Issued: "2026-06-14", ID: "NL-1"})
	c, err := Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.Customer != "Acme Ltd" || c.Tier != TierBusiness || c.Seats != 50 {
		t.Errorf("claims = %+v", c)
	}
}

func TestVerifyRejectsTamper(t *testing.T) {
	tok := issue(t, Claims{Customer: "Acme Ltd", Tier: TierBusiness, ID: "NL-1"})
	// Flip the customer in the payload but keep the original signature.
	forged, _ := json.Marshal(Claims{Customer: "Acme Ltd PIRATE", Tier: TierBusiness, ID: "NL-1"})
	parts := []byte(tok)
	_ = parts
	bad := "NIMBO-LIC-1." + base64.RawURLEncoding.EncodeToString(forged) + "." + tok[len(tok)-43:]
	if _, err := Verify(bad); err == nil {
		t.Fatal("tampered payload accepted")
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	// A token signed by some OTHER key (not the embedded one) must fail.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	payload, _ := json.Marshal(Claims{Customer: "Nobody", Tier: TierBusiness})
	sig := ed25519.Sign(priv, payload)
	tok := "NIMBO-LIC-1." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
	if _, err := Verify(tok); err == nil {
		t.Fatal("token signed by an untrusted key was accepted")
	}
}

func TestVerifyMalformed(t *testing.T) {
	for _, tok := range []string{"", "garbage", "NIMBO-LIC-1.onlyonepart", "NIMBO-LIC-1.!!!.@@@"} {
		if _, err := Verify(tok); err == nil {
			t.Errorf("Verify(%q) accepted a malformed token", tok)
		}
	}
}

func TestEvaluateExpiry(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

	// Perpetual (no expiry) → licensed.
	tok := issue(t, Claims{Customer: "Acme", Tier: TierBusiness})
	if in := Evaluate(tok, now); !in.Licensed || in.Expired {
		t.Errorf("perpetual: %+v", in)
	}

	// Future expiry → licensed.
	tok = issue(t, Claims{Customer: "Acme", Tier: TierBusiness, Expires: "2027-01-01"})
	if in := Evaluate(tok, now); !in.Licensed {
		t.Errorf("future expiry: %+v", in)
	}

	// Past expiry → installed but not licensed, flagged expired.
	tok = issue(t, Claims{Customer: "Acme", Tier: TierBusiness, Expires: "2026-01-01"})
	in := Evaluate(tok, now)
	if in.Licensed || !in.Expired || !in.HasLicence {
		t.Errorf("past expiry: %+v", in)
	}
}

func TestEvaluateEmpty(t *testing.T) {
	in := Evaluate("  ", time.Now())
	if in.HasLicence || in.Licensed || in.Err != "" {
		t.Errorf("empty token should be the clean unlicensed state, got %+v", in)
	}
}

func TestAllows(t *testing.T) {
	// Personal is always allowed; business needs a valid business licence.
	none := Info{}
	if !none.Allows(TierPersonal) {
		t.Error("personal tier should always be allowed")
	}
	if none.Allows(TierBusiness) {
		t.Error("unlicensed must not allow business")
	}
	biz := Info{Licensed: true, Tier: TierBusiness}
	if !biz.Allows(TierBusiness) {
		t.Error("valid business licence should allow business")
	}
	expired := Info{Licensed: false, Tier: TierBusiness, Expired: true}
	if expired.Allows(TierBusiness) {
		t.Error("expired business licence must not allow business")
	}
}

func TestSaveLoadRemove(t *testing.T) {
	p := t.TempDir() + "/license.key"
	if Load(p) != "" {
		t.Error("missing file should load as empty")
	}
	if err := Save(p, "  NIMBO-LIC-1.abc.def \n"); err != nil {
		t.Fatal(err)
	}
	if got := Load(p); got != "NIMBO-LIC-1.abc.def" {
		t.Errorf("round-trip = %q", got)
	}
	if err := Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := Remove(p); err != nil {
		t.Errorf("Remove on absent file should be nil, got %v", err)
	}
}
