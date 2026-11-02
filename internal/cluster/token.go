package cluster

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// JoinToken is what an operator copies from the leader to a new node. The secret is shown once
// and only its SHA-256 hash is stored. The CA fingerprint lets the new node authenticate the
// cluster before it has the CA certificate (trust on first use is never required).
type JoinToken struct {
	ClusterID     string   `json:"c"`
	TokenID       string   `json:"i"`
	Secret        string   `json:"s"`
	CAFingerprint string   `json:"f"`
	Addrs         []string `json:"a"`
}

const tokenPrefix = "IBJ1."

func (t *JoinToken) String() string {
	b, _ := json.Marshal(t)
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// ParseJoinToken decodes and validates the token format.
func ParseJoinToken(s string) (*JoinToken, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, tokenPrefix) || len(s) > 4096 {
		return nil, errors.New("not an Intrabin join token")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, tokenPrefix))
	if err != nil {
		return nil, errors.New("join token is malformed")
	}
	var t JoinToken
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, errors.New("join token is malformed")
	}
	if t.TokenID == "" || len(t.Secret) < 32 || len(t.CAFingerprint) != 64 || len(t.Addrs) == 0 {
		return nil, errors.New("join token is incomplete")
	}
	return &t, nil
}
