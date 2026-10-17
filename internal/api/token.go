package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Tokens are "<prefix>_<id>_<secret>". The prefix lets secret scanners spot
// them; the id is what the server looks up, the secret is compared by hash.
// API tokens are ssk_, Node Tokens ssn_, Enrollment Packets sse_. A Sandbox
// Token is sst_, and its id is the Sandbox id (api.md §1).
const (
	apiTokenPrefix         = "ssk_"
	nodeTokenPrefix        = "ssn_"
	enrollmentPacketPrefix = "sse_"
	sandboxTokenPrefix     = "sst_"
)

type TokenParts struct {
	ID     uuid.UUID
	Secret string
}

func ParseToken(raw string) (TokenParts, bool) {
	return parsePrefixedToken(apiTokenPrefix, raw)
}

func ParseNodeToken(raw string) (TokenParts, bool) {
	return parsePrefixedToken(nodeTokenPrefix, raw)
}

func ParseEnrollmentPacket(raw string) (TokenParts, bool) {
	return parsePrefixedToken(enrollmentPacketPrefix, raw)
}

func ParseSandboxToken(raw string) (TokenParts, bool) {
	return parsePrefixedToken(sandboxTokenPrefix, raw)
}

// SandboxToken is the wire form of a Sandbox Token.
func SandboxToken(id uuid.UUID, secret string) string {
	return sandboxTokenPrefix + id.String() + "_" + secret
}

func parsePrefixedToken(prefix, raw string) (TokenParts, bool) {
	rest, ok := strings.CutPrefix(raw, prefix)
	if !ok {
		return TokenParts{}, false
	}
	idPart, secret, ok := strings.Cut(rest, "_")
	if !ok || secret == "" {
		return TokenParts{}, false
	}
	id, err := uuid.Parse(idPart)
	if err != nil || id.String() != idPart {
		return TokenParts{}, false
	}
	return TokenParts{ID: id, Secret: secret}, true
}

func NewToken() (raw string, id uuid.UUID, hash []byte, err error) {
	return newPrefixedToken(apiTokenPrefix)
}

func NewNodeToken() (raw string, id uuid.UUID, hash []byte, err error) {
	return newPrefixedToken(nodeTokenPrefix)
}

func NewEnrollmentPacket() (raw string, id uuid.UUID, hash []byte, err error) {
	return newPrefixedToken(enrollmentPacketPrefix)
}

func newPrefixedToken(prefix string) (raw string, id uuid.UUID, hash []byte, err error) {
	id, err = uuid.NewV7()
	if err != nil {
		return "", uuid.UUID{}, nil, err
	}
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", uuid.UUID{}, nil, err
	}
	secret := base64.RawURLEncoding.EncodeToString(buf)
	return prefix + id.String() + "_" + secret, id, HashSecret(secret), nil
}

func HashSecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// EncodeCursor and DecodeCursor keep pagination cursors opaque; callers must
// not build one or infer order from it.
func EncodeCursor(id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id.String()))
}

func DecodeCursor(cursor string) (uuid.UUID, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return uuid.UUID{}, false
	}
	id, err := uuid.Parse(string(raw))
	if err != nil || id.String() != string(raw) {
		return uuid.UUID{}, false
	}
	return id, true
}

// Audit records use a bigint id, so their cursor is opaque in the same way.
func EncodeIntCursor(n int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(n, 10)))
}

func DecodeIntCursor(cursor string) (int64, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
