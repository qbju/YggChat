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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"strings"

	"golang.org/x/crypto/scrypt"
)

const (
	Chunk   = 864              // raw bytes per datagram chunk (same as images)
	MaxSize = 64 * 1024 * 1024 // sealed bundle
	MaxN    = (MaxSize + Chunk - 1) / Chunk
	maxGz   = 256 * 1024 * 1024 // decompressed bundle
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
	if len(pass) < 4 {
		return nil, errors.New("パスフレーズは4文字以上にしてください")
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
