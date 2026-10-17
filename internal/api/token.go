package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// API tokens are "ssk_<id>_<secret>". The prefix lets secret scanners spot
// them; the id is what the server looks up, the secret is compared by hash.
const apiTokenPrefix = "ssk_"

type TokenParts struct {
	ID     uuid.UUID
	Secret string
}

func ParseToken(raw string) (TokenParts, bool) {
	rest, ok := strings.CutPrefix(raw, apiTokenPrefix)
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
	id, err = uuid.NewV7()
	if err != nil {
		return "", uuid.UUID{}, nil, err
	}
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", uuid.UUID{}, nil, err
	}
	secret := base64.RawURLEncoding.EncodeToString(buf)
	return apiTokenPrefix + id.String() + "_" + secret, id, HashSecret(secret), nil
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
