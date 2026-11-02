package security

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var fastParams = Argon2Params{Memory: 1024, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16}

func TestPasswordHashRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery", fastParams)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$") {
		t.Fatalf("unexpected format %s", h)
	}
	ok, err := VerifyPassword("correct horse battery", h)
	if err != nil || !ok {
		t.Fatalf("expected match, got %v %v", ok, err)
	}
	ok, _ = VerifyPassword("wrong password!", h)
	if ok {
		t.Fatal("wrong password matched")
	}
	h2, _ := HashPassword("correct horse battery", fastParams)
	if h == h2 {
		t.Fatal("salt not random")
	}
}

func TestPasswordPolicy(t *testing.T) {
	if ValidatePassword("short") == nil {
		t.Fatal("short password accepted")
	}
	if ValidatePassword("long enough pw") != nil {
		t.Fatal("valid password rejected")
	}
}

func TestSecretBox(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	sb, err := NewSecretBox(key)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := sb.Seal([]byte("s3cret"), []byte("monitor:1"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("s3cret")) {
		t.Fatal("plaintext visible in ciphertext")
	}
	pt, err := sb.Open(ct, []byte("monitor:1"))
	if err != nil || string(pt) != "s3cret" {
		t.Fatalf("roundtrip failed: %v", err)
	}
	if _, err := sb.Open(ct, []byte("monitor:2")); err == nil {
		t.Fatal("AAD not enforced")
	}
	other, _ := NewSecretBox(bytes.Repeat([]byte{8}, 32))
	if _, err := other.Open(ct, []byte("monitor:1")); err == nil {
		t.Fatal("wrong key decrypted")
	}
}

func TestMasterKeyFile(t *testing.T) {
	dir := t.TempDir()
	k1, created, err := LoadOrCreateMasterKey("", dir)
	if err != nil || !created || len(k1) != 32 {
		t.Fatalf("create: %v %v", created, err)
	}
	st, _ := os.Stat(filepath.Join(dir, "master.key"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("bad perms %v", st.Mode())
	}
	k2, created, err := LoadOrCreateMasterKey("", dir)
	if err != nil || created || !bytes.Equal(k1, k2) {
		t.Fatal("reload mismatch")
	}
	if _, _, err := LoadOrCreateMasterKey("nothex", dir); err == nil {
		t.Fatal("invalid env key accepted")
	}
}

func TestRedact(t *testing.T) {
	got := RedactString("GET https://user:pw@example.com/x failed")
	if strings.Contains(got, "pw@") {
		t.Fatal(got)
	}
	m := RedactMap(map[string]string{"Authorization": "Bearer x", "Accept": "json"})
	if m["Authorization"] != "[REDACTED]" || m["Accept"] != "json" {
		t.Fatal(m)
	}
}
