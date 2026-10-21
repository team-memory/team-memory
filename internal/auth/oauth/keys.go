package oauth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/zitadel/oidc/v3/pkg/op"
)

// signingKey is one private key loaded from the signing-keys directory.
type signingKey struct {
	id   string
	alg  jose.SignatureAlgorithm
	priv crypto.Signer
}

func (k *signingKey) SignatureAlgorithm() jose.SignatureAlgorithm { return k.alg }
func (k *signingKey) Key() any                                    { return k.priv }
func (k *signingKey) ID() string                                  { return k.id }

// publicKey adapts a signing key's public half to op.Key for the JWKS.
type publicKey struct{ k *signingKey }

func (p publicKey) ID() string                         { return p.k.id }
func (p publicKey) Algorithm() jose.SignatureAlgorithm { return p.k.alg }
func (p publicKey) Use() string                        { return "sig" }
func (p publicKey) Key() any                           { return p.k.priv.Public() }

// Keyring holds every configured key. All are published and accepted for
// verification; only the active key signs. Rotation: add the new key file,
// switch active_signing_key once every replica has it, and delete the old
// file after the longest access-token lifetime has passed.
type Keyring struct {
	active *signingKey
	byID   map[string]*signingKey
}

var keyID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// LoadKeyring reads <kid>.pem files (PKCS#8, SEC 1 EC or PKCS#1 RSA private
// keys; EC keys must be P-256, RSA keys at least 2048 bits).
func LoadKeyring(dir, active string) (*Keyring, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.pem"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("signing keys: no *.pem files in %s", dir)
	}
	sort.Strings(paths)
	kr := &Keyring{byID: map[string]*signingKey{}}
	for _, p := range paths {
		id := strings.TrimSuffix(filepath.Base(p), ".pem")
		if !keyID.MatchString(id) {
			return nil, fmt.Errorf("signing keys: file name %q is not a valid key id", filepath.Base(p))
		}
		k, err := readKey(p)
		if err != nil {
			return nil, fmt.Errorf("signing keys: %s: %w", filepath.Base(p), err)
		}
		k.id = id
		kr.byID[id] = k
	}
	switch {
	case active != "":
		kr.active = kr.byID[active]
		if kr.active == nil {
			return nil, fmt.Errorf("signing keys: active_signing_key %q has no %s.pem in %s", active, active, dir)
		}
	case len(kr.byID) == 1:
		for _, k := range kr.byID {
			kr.active = k
		}
	default:
		return nil, errors.New("signing keys: several keys present; set active_signing_key")
	}
	return kr, nil
}

func readKey(path string) (*signingKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("not PEM")
	}
	var priv any
	switch block.Type {
	case "PRIVATE KEY":
		priv, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		priv, err = x509.ParseECPrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		priv, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("unsupported PEM block %q", block.Type)
	}
	if err != nil {
		return nil, err
	}
	switch k := priv.(type) {
	case *ecdsa.PrivateKey:
		if k.Curve != elliptic.P256() {
			return nil, errors.New("EC keys must use P-256")
		}
		return &signingKey{alg: jose.ES256, priv: k}, nil
	case *rsa.PrivateKey:
		if k.N.BitLen() < 2048 {
			return nil, errors.New("RSA keys must be at least 2048 bits")
		}
		return &signingKey{alg: jose.RS256, priv: k}, nil
	}
	return nil, fmt.Errorf("unsupported key type %T", priv)
}

// Active returns the signing key.
func (kr *Keyring) Active() op.SigningKey { return kr.active }

// Public returns every key for the JWKS.
func (kr *Keyring) Public() []op.Key {
	out := make([]op.Key, 0, len(kr.byID))
	for _, k := range kr.byID {
		out = append(out, publicKey{k})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// Algorithms lists the signature algorithms of all keys.
func (kr *Keyring) Algorithms() []jose.SignatureAlgorithm {
	var out []jose.SignatureAlgorithm
	for _, k := range kr.byID {
		if !slices.Contains(out, k.alg) {
			out = append(out, k.alg)
		}
	}
	return out
}

func (kr *Keyring) verificationKey(kid string) (any, jose.SignatureAlgorithm, bool) {
	k, ok := kr.byID[kid]
	if !ok {
		return nil, "", false
	}
	return k.priv.Public(), k.alg, true
}

// derivedKey returns 32 bytes derived from the active signing key, used to
// encrypt authorization codes. It changes when the active key rotates,
// which invalidates only codes still in flight (minutes).
func (kr *Keyring) derivedKey(purpose string) [32]byte {
	der, _ := x509.MarshalPKCS8PrivateKey(kr.active.priv)
	return sha256.Sum256(append([]byte("team-memory/"+purpose+"/"), der...))
}
