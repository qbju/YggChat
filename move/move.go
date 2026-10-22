// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Package move: moving an account (key + data) to another device, shared by
// the native node and the WASM/PWA node.
//
// Direct transfer: the new device makes a throwaway X25519 key and shows a
// code "ygm1:<its ygg address>:<x25519 pubkey>". The old device seals the
// bundle to that key (ephemeral X25519 + HKDF-ish SHA-256 + AES-256-GCM) and
// sends it over Yggdrasil in chunks. Only the device holding the throwaway
// private key can open it, and the Yggdrasil address is bound to the
// receiver's node key, so nobody in between can read or redirect it.
//
// Backup file: the same bundle sealed with a passphrase (scrypt + AES-GCM).
package move

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/scrypt"
)

const (
	Chunk   = 864              // raw bytes per datagram chunk (same as images)
	MaxSize = 64 * 1024 * 1024 // sealed bundle
	MaxN    = (MaxSize + Chunk - 1) / Chunk
	maxGz   = 256 * 1024 * 1024 // decompressed bundle
	MinPass = 8                 // backup passphrase, in characters
)

var b64u = base64.RawURLEncoding

// NewReceiver makes the throwaway key for one transfer and the code to show.
func NewReceiver(addr string) (*ecdh.PrivateKey, string, error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	return k, "ygm1:" + addr + ":" + b64u.EncodeToString(k.PublicKey().Bytes()), nil
}

// ParseCode returns the receiver's Yggdrasil address and X25519 public key.
func ParseCode(s string) (string, []byte, error) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "ygm1:"); i >= 0 {
		s = s[i:]
	}
	rest, ok := strings.CutPrefix(s, "ygm1:")
	j := strings.LastIndex(rest, ":")
	if !ok || j < 0 {
		return "", nil, errors.New("移行コードではありません")
	}
	ip := net.ParseIP(rest[:j])
	pub, err := b64u.DecodeString(strings.TrimSpace(rest[j+1:]))
	if ip == nil || ip.To4() != nil || ip[0]&0xfe != 0x02 || err != nil || len(pub) != 32 {
		return "", nil, errors.New("移行コードが壊れています")
	}
	return ip.String(), pub, nil
}

// Transfer authentication (v0.19.4). The receiver's X25519 public key only
// ever travels inside the code (QR or typed); it is never sent over the mesh.
// So it doubles as a shared secret between the code's holder and the
// receiver: the sender adds MAC(AuthKey(rpk), "mh", id, size, n, sha) to its
// header ("ma"), and the receiver gives its single transfer slot to an
// authenticated header even if another node holds it. Without this any mesh
// node could take or hog the slot while the receive screen was open.
// Codes and the bundle format are unchanged, so old peers still interoperate
// (an old sender sends no "ma" and gets the old first-come slot).
func AuthKey(rpk []byte) []byte {
	h := sha256.New()
	h.Write([]byte("yggchat-move-auth1\n"))
	h.Write(rpk)
	return h.Sum(nil)
}

// MAC is HMAC-SHA256 over the newline-joined parts, 16 bytes, hex.
func MAC(key []byte, parts ...string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(m.Sum(nil)[:16])
}

// HeaderMAC authenticates a header {id,size,n,sha}.
func HeaderMAC(key []byte, id string, size, n int, sha string) string {
	return MAC(key, "mh", id, strconv.Itoa(size), strconv.Itoa(n), sha)
}

// ReplyMAC authenticates the receiver's answer to a header.
func ReplyMAC(key []byte, id string) string { return MAC(key, "mha", id) }

// MACOK compares in constant time.
func MACOK(want, got string) bool {
	return len(got) == 32 && hmac.Equal([]byte(want), []byte(got))
}

func gz(b []byte) []byte {
	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func gunzip(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	out, err := io.ReadAll(io.LimitReader(r, maxGz+1))
	if err == nil && len(out) > maxGz {
		err = errors.New("too large")
	}
	return out, err
}

func gcmSeal(key, plain, ad []byte) []byte {
	blk, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(blk)
	nonce := make([]byte, g.NonceSize())
	rand.Read(nonce)
	return g.Seal(nonce, nonce, plain, ad)
}

func gcmOpen(key, ct, ad []byte) ([]byte, error) {
	blk, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(blk)
	if len(ct) < g.NonceSize()+g.Overhead() {
		return nil, errors.New("short")
	}
	return g.Open(nil, ct[:g.NonceSize()], ct[g.NonceSize():], ad)
}

func boxKey(shared, epk, rpk []byte) []byte {
	h := sha256.New()
	h.Write([]byte("yggchat-move-v1"))
	h.Write(shared)
	h.Write(epk)
	h.Write(rpk)
	return h.Sum(nil)
}

// Seal compresses and encrypts a bundle to the receiver's X25519 key.
// Output: epk(32) || nonce(12) || ciphertext.
func Seal(rpk, plain []byte) ([]byte, error) {
	pub, err := ecdh.X25519().NewPublicKey(rpk)
	if err != nil {
		return nil, err
	}
	e, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := e.ECDH(pub)
	if err != nil {
		return nil, err
	}
	epk := e.PublicKey().Bytes()
	out := append([]byte{}, epk...)
	out = append(out, gcmSeal(boxKey(shared, epk, rpk), gz(plain), []byte("ygm1"))...)
	if len(out) > MaxSize {
		return nil, errors.New("データが大きすぎます（64MBまで）。履歴を外して試してください")
	}
	return out, nil
}

// Open decrypts a sealed bundle with the receiver's throwaway key.
func Open(k *ecdh.PrivateKey, blob []byte) ([]byte, error) {
	if len(blob) < 32 {
		return nil, errors.New("壊れたデータです")
	}
	pub, err := ecdh.X25519().NewPublicKey(blob[:32])
	if err != nil {
		return nil, err
	}
	shared, err := k.ECDH(pub)
	if err != nil {
		return nil, err
	}
	z, err := gcmOpen(boxKey(shared, blob[:32], k.PublicKey().Bytes()), blob[32:], []byte("ygm1"))
	if err != nil {
		return nil, errors.New("復号できませんでした")
	}
	return gunzip(z)
}

// Backup file: "YGB1" || salt(16) || nonce(12) || ciphertext.
func SealPass(pass string, plain []byte) ([]byte, error) {
	// The file holds the identity key and history and can be attacked offline,
	// so a 4-byte minimum (two Japanese characters) was far too weak.
	if utf8.RuneCountInString(strings.TrimSpace(pass)) < MinPass {
		return nil, errors.New("パスフレーズは8文字以上にしてください")
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	k, err := scrypt.Key([]byte(pass), salt, 1<<15, 8, 1, 32)
	if err != nil {
		return nil, err
	}
	out := append([]byte("YGB1"), salt...)
	return append(out, gcmSeal(k, gz(plain), []byte("YGB1"))...), nil
}

func OpenPass(pass string, blob []byte) ([]byte, error) {
	if len(blob) < 20 || string(blob[:4]) != "YGB1" {
		return nil, errors.New("YggChatのバックアップファイルではありません")
	}
	k, err := scrypt.Key([]byte(pass), blob[4:20], 1<<15, 8, 1, 32)
	if err != nil {
		return nil, err
	}
	z, err := gcmOpen(k, blob[20:], []byte("YGB1"))
	if err != nil {
		return nil, errors.New("パスフレーズが違うか、ファイルが壊れています")
	}
	return gunzip(z)
}
