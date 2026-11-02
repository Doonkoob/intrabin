package cluster

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"time"

	"crypto/aes"
	"crypto/cipher"

	"golang.org/x/crypto/hkdf"
)

// PeerServerName is a DNS SAN present in every node certificate. Peers verify each other
// against the cluster CA and this name (node addresses change; identity comes from the CA).
const PeerServerName = "intrabin-node"

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

func pemEncode(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func pemDecode(b []byte, typ string) ([]byte, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != typ {
		return nil, fmt.Errorf("expected PEM %s", typ)
	}
	return blk.Bytes, nil
}

// NewCA creates a self-signed cluster CA (ECDSA P-256, 10 years).
func NewCA(clusterID string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "Intrabin cluster CA " + clusterID, Organization: []string{"Intrabin"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kd, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pemEncode("CERTIFICATE", der), pemEncode("PRIVATE KEY", kd), nil
}

// NewNodeKey creates a node private key and a CSR for it.
func NewNodeKey(nodeID string) (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: nodeID}}, key)
	if err != nil {
		return nil, nil, err
	}
	kd, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pemEncode("PRIVATE KEY", kd), pemEncode("CERTIFICATE REQUEST", csr), nil
}

// SignNode issues a node certificate (1 year, client+server auth). The subject is forced to
// nodeID regardless of what the CSR requested.
func SignNode(caCertPEM, caKeyPEM, csrPEM []byte, nodeID string, hosts []string) ([]byte, error) {
	caDer, err := pemDecode(caCertPEM, "CERTIFICATE")
	if err != nil {
		return nil, err
	}
	ca, err := x509.ParseCertificate(caDer)
	if err != nil {
		return nil, err
	}
	kd, err := pemDecode(caKeyPEM, "PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	caKey, err := x509.ParsePKCS8PrivateKey(kd)
	if err != nil {
		return nil, err
	}
	cd, err := pemDecode(csrPEM, "CERTIFICATE REQUEST")
	if err != nil {
		return nil, err
	}
	csr, err := x509.ParseCertificateRequest(cd)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("invalid CSR signature: %w", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: nodeID, Organization: []string{"Intrabin"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{PeerServerName, "node-" + nodeID + ".intrabin"},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else if h != "" {
			tpl.DNSNames = append(tpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, csr.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	return pemEncode("CERTIFICATE", der), nil
}

// Fingerprint returns the hex SHA-256 of a PEM certificate's DER bytes.
func Fingerprint(certPEM []byte) (string, error) {
	der, err := pemDecode(certPEM, "CERTIFICATE")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

// ---- cluster data key transfer ----
//
// The joining node sends an ephemeral X25519 public key. The leader derives a shared AES-256-GCM
// key (HKDF-SHA256) and returns the cluster data key encrypted under it, so the data key never
// appears in plaintext in any request or response body (in addition to the TLS channel).

// WrappedKey is an encrypted cluster data key.
type WrappedKey struct {
	EphemeralPub []byte `json:"epub"`
	Nonce        []byte `json:"nonce"`
	Ciphertext   []byte `json:"ct"`
}

func deriveWrapKey(shared []byte, info string) ([]byte, error) {
	k := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, nil, []byte(info)), k); err != nil {
		return nil, err
	}
	return k, nil
}

// WrapKey encrypts dataKey to the recipient's X25519 public key.
func WrapKey(recipientPub []byte, dataKey []byte, info string) (*WrappedKey, error) {
	pub, err := ecdh.X25519().NewPublicKey(recipientPub)
	if err != nil {
		return nil, err
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := eph.ECDH(pub)
	if err != nil {
		return nil, err
	}
	k, err := deriveWrapKey(shared, info)
	if err != nil {
		return nil, err
	}
	blk, _ := aes.NewCipher(k)
	gcm, _ := cipher.NewGCM(blk)
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return &WrappedKey{EphemeralPub: eph.PublicKey().Bytes(), Nonce: nonce, Ciphertext: gcm.Seal(nil, nonce, dataKey, []byte(info))}, nil
}

// UnwrapKey reverses WrapKey with the recipient's private key.
func UnwrapKey(priv *ecdh.PrivateKey, w *WrappedKey, info string) ([]byte, error) {
	if w == nil {
		return nil, errors.New("missing wrapped key")
	}
	pub, err := ecdh.X25519().NewPublicKey(w.EphemeralPub)
	if err != nil {
		return nil, err
	}
	shared, err := priv.ECDH(pub)
	if err != nil {
		return nil, err
	}
	k, err := deriveWrapKey(shared, info)
	if err != nil {
		return nil, err
	}
	blk, _ := aes.NewCipher(k)
	gcm, _ := cipher.NewGCM(blk)
	return gcm.Open(nil, w.Nonce, w.Ciphertext, []byte(info))
}
