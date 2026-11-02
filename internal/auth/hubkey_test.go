package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseHubPublicKeyAcceptsInlinePEMAndBase64(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	pkixPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	rawPEM := pem.EncodeToMemory(&pem.Block{Type: "ED25519 PUBLIC KEY", Bytes: pub})
	encoded := base64.StdEncoding.EncodeToString(pub)

	cases := map[string][]byte{
		"pkix pem":       pkixPEM,
		"raw ed25519":    rawPEM,
		"base64":         []byte(encoded),
		"wrapped base64": []byte(encoded[:20] + "\n" + encoded[20:]),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseHubPublicKey(raw)
			if err != nil {
				t.Fatalf("ParseHubPublicKey: %v", err)
			}
			if !got.Equal(pub) {
				t.Error("parsed key differs from the original")
			}
		})
	}
}

// 字段语义是内容，不是路径：即使路径指向一个真实存在的公钥文件，
// 也必须报错，不能悄悄读文件。
func TestParseHubPublicKeyRejectsFilePath(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "homehub.pub")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = ParseHubPublicKey([]byte(path))
	if err == nil {
		t.Fatal("a file path must not be read as a public key")
	}
	if !strings.Contains(err.Error(), "not a path") {
		t.Errorf("error should say the field holds the key itself: %v", err)
	}
}
