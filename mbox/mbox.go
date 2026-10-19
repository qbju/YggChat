// Package mbox: sealed boxes for the YggChat relay mailbox (shared by the
// native node and the WASM/PWA node).
package mbox

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net"

	"github.com/yggdrasil-network/yggdrasil-go/src/address"
	"golang.org/x/crypto/nacl/box"
)

const Base = "https://yggws.i2p.workers.dev/mb/"

var p25519, _ = new(big.Int).SetString("7fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffed", 16)

// edPubToX converts an Ed25519 public key to its X25519 (Montgomery u) form:
// u = (1+y)/(1-y) mod p.
func edPubToX(pk []byte) (*[32]byte, error) {
	if len(pk) != 32 {
		return nil, errors.New("bad key")
	}
	le := append([]byte{}, pk...)
	le[31] &= 0x7f
	be := make([]byte, 32)
	for i := range le {
		be[31-i] = le[i]
	}
	y := new(big.Int).SetBytes(be)
	one := big.NewInt(1)
	num := new(big.Int).Add(one, y)
	den := new(big.Int).Sub(one, y)
	den.Mod(den, p25519)
	if den.Sign() == 0 {
		return nil, errors.New("bad key")
	}
	den.ModInverse(den, p25519)
	u := num.Mul(num, den)
	u.Mod(u, p25519)
	ub := u.FillBytes(make([]byte, 32))
	var out [32]byte
	for i := range ub {
		out[i] = ub[31-i]
	}
	return &out, nil
}

func edPrivToX(priv ed25519.PrivateKey) *[32]byte {
	h := sha512.Sum512(priv.Seed())
	var s [32]byte
	copy(s[:], h[:32])
	s[0] &= 248
	s[31] &= 127
	s[31] |= 64
	return &s
}

func Tag(pk []byte) string {
	h := sha256.Sum256([]byte("yggchat-mb:" + hex.EncodeToString(pk)))
	return hex.EncodeToString(h[:])[:32]
}

type mbInner struct {
	PK  string          `json:"pk"`  // sender Ed25519 public key (hex)
	W   json.RawMessage `json:"w"`   // the wire packet
	Sig string          `json:"sig"` // sign(recipientPK || w)
}

func Seal(to ed25519.PublicKey, priv ed25519.PrivateKey, wb []byte) (string, error) {
	xb, err := edPubToX(to)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(priv, append(append([]byte{}, to...), wb...))
	inner, _ := json.Marshal(mbInner{PK: hex.EncodeToString(priv.Public().(ed25519.PublicKey)), W: wb, Sig: hex.EncodeToString(sig)})
	sealed, err := box.SealAnonymous(nil, inner, xb, rand.Reader)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func Open(b64 string, priv ed25519.PrivateKey) (from string, pk ed25519.PublicKey, wb []byte, err error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return
	}
	mypub := priv.Public().(ed25519.PublicKey)
	xpub, err := edPubToX(mypub)
	if err != nil {
		return
	}
	plain, ok := box.OpenAnonymous(nil, raw, xpub, edPrivToX(priv))
	if !ok {
		err = errors.New("cannot open box")
		return
	}
	var in mbInner
	if err = json.Unmarshal(plain, &in); err != nil {
		return
	}
	pkb, _ := hex.DecodeString(in.PK)
	sig, _ := hex.DecodeString(in.Sig)
	if len(pkb) != ed25519.PublicKeySize || !ed25519.Verify(pkb, append(append([]byte{}, mypub...), in.W...), sig) {
		err = errors.New("bad signature")
		return
	}
	return net.IP(address.AddrForKey(pkb)[:]).String(), pkb, in.W, nil
}


// AddrOK reports whether pk is the key behind Yggdrasil address addr.
func AddrOK(addr string, pk []byte) bool {
	return len(pk) == ed25519.PublicKeySize && net.IP(address.AddrForKey(pk)[:]).String() == addr
}
