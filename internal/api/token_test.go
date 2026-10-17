package api

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestParseToken(t *testing.T) {
	id := uuid.MustParse("0192f0a0-0000-7000-8000-000000000001")
	secret := strings.Repeat("a", 43)

	parts, ok := ParseToken("ssk_" + id.String() + "_" + secret)
	if !ok {
		t.Fatal("ParseToken rejected a well-formed token")
	}
	if parts.ID != id {
		t.Fatalf("id = %v, want %v", parts.ID, id)
	}
	if parts.Secret != secret {
		t.Fatalf("secret = %q, want %q", parts.Secret, secret)
	}

	// base64url secrets contain '_' roughly half the time.
	underscored := "ab_cd"
	if parts, ok := ParseToken("ssk_" + id.String() + "_" + underscored); !ok || parts.Secret != underscored {
		t.Fatalf("ParseToken rejected an underscore in the secret: %v, %v", parts, ok)
	}

	for _, raw := range []string{
		"",
		"ssk_",
		"ssk_" + id.String(),
		"ssk_" + id.String() + "_",
		"sst_" + id.String() + "_" + secret,
		"ssk_not-a-uuid_" + secret,
		"Bearer " + "ssk_" + id.String() + "_" + secret,
	} {
		if _, ok := ParseToken(raw); ok {
			t.Errorf("ParseToken(%q) accepted a malformed token", raw)
		}
	}
}

func TestParseNodeTokenAndEnrollmentPacket(t *testing.T) {
	id := uuid.MustParse("0192f0a0-0000-7000-8000-000000000002")
	secret := strings.Repeat("b", 43)

	parts, ok := ParseNodeToken("ssn_" + id.String() + "_" + secret)
	if !ok || parts.ID != id || parts.Secret != secret {
		t.Fatalf("ParseNodeToken = %+v, %v", parts, ok)
	}
	if _, ok := ParseNodeToken("ssk_" + id.String() + "_" + secret); ok {
		t.Fatal("ParseNodeToken accepted an API Token prefix")
	}

	parts, ok = ParseEnrollmentPacket("sse_" + id.String() + "_" + secret)
	if !ok || parts.ID != id || parts.Secret != secret {
		t.Fatalf("ParseEnrollmentPacket = %+v, %v", parts, ok)
	}
	if _, ok := ParseEnrollmentPacket("ssn_" + id.String() + "_" + secret); ok {
		t.Fatal("ParseEnrollmentPacket accepted a Node Token prefix")
	}
}

func TestTokenHashIsStableAndSecretOnly(t *testing.T) {
	a := HashSecret("secret")
	b := HashSecret("secret")
	if string(a) != string(b) {
		t.Fatal("HashSecret is not deterministic")
	}
	if len(a) != 32 {
		t.Fatalf("hash length = %d, want 32", len(a))
	}
	if string(HashSecret("other")) == string(a) {
		t.Fatal("different secrets hashed the same")
	}
}

func TestCursorRoundTrip(t *testing.T) {
	id := uuid.MustParse("0192f0a0-0000-7000-8000-0000000000ff")
	cur := EncodeCursor(id)
	if cur == id.String() {
		t.Fatal("cursor is not opaque")
	}
	got, ok := DecodeCursor(cur)
	if !ok || got != id {
		t.Fatalf("DecodeCursor(%q) = %v, %v; want %v, true", cur, got, ok, id)
	}
	for _, bad := range []string{"", "!!!", "bm90LWEtdXVpZA"} {
		if _, ok := DecodeCursor(bad); ok {
			t.Errorf("DecodeCursor(%q) accepted garbage", bad)
		}
	}

	intCur := EncodeIntCursor(42)
	if intCur == "42" {
		t.Fatal("int cursor is not opaque")
	}
	if got, ok := DecodeIntCursor(intCur); !ok || got != 42 {
		t.Fatalf("DecodeIntCursor(%q) = %d, %v; want 42, true", intCur, got, ok)
	}
	for _, bad := range []string{"", "!!!", EncodeIntCursor(0), "MHNlay0"} {
		if _, ok := DecodeIntCursor(bad); ok {
			t.Errorf("DecodeIntCursor(%q) accepted garbage", bad)
		}
	}
}
