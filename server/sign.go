package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
)

const (
	HeaderKey = "Layer8-Key"
	HeaderSig = "Layer8-Signature"
)

// decodeB64 accepts standard or URL-safe base64, padded or not.
// Agents produce all four and none of them are worth a rejection.
func decodeB64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "=")
	s = strings.NewReplacer("+", "-", "/", "_").Replace(s)
	return base64.RawURLEncoding.DecodeString(s)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

var (
	errKeyLen = errors.New("public key must be the raw 32-byte Ed25519 key, base64 encoded")
	errSigLen = errors.New("signature must be the 64-byte Ed25519 signature, base64 encoded")
)

var errWeakKey = errors.New("that public key is a small-order point, which can sign anything; generate a real keypair")

func parseKey(s string) (ed25519.PublicKey, error) {
	b, err := decodeB64(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errKeyLen
	}
	if smallOrder(b) {
		return nil, errWeakKey
	}
	return ed25519.PublicKey(b), nil
}

// Small-order Ed25519 points have y in {0, 1, p-1, y8, p-y8}. A key on one of
// them verifies forged signatures, so a receipt signed by it proves nothing.
var (
	fieldP     = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	smallOrdYs = func() []*big.Int {
		y8, _ := new(big.Int).SetString("2707385501144840649318225287225658788936804267575313519463743609750303402022", 10)
		return []*big.Int{
			big.NewInt(0),
			big.NewInt(1),
			new(big.Int).Sub(fieldP, big.NewInt(1)),
			y8,
			new(big.Int).Sub(fieldP, y8),
		}
	}()
)

func smallOrder(key []byte) bool {
	le := make([]byte, 32)
	copy(le, key)
	le[31] &= 0x7f // drop the sign bit of x
	for i, j := 0, 31; i < j; i, j = i+1, j-1 {
		le[i], le[j] = le[j], le[i]
	}
	y := new(big.Int).Mod(new(big.Int).SetBytes(le), fieldP)
	for _, s := range smallOrdYs {
		if y.Cmp(s) == 0 {
			return true
		}
	}
	return false
}

func parseSig(s string) ([]byte, error) {
	b, err := decodeB64(s)
	if err != nil || len(b) != ed25519.SignatureSize {
		return nil, errSigLen
	}
	return b, nil
}

// keyID is the public name of a key, and so of the human behind it.
func keyID(pub []byte) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

var (
	callAdj = []string{
		"Amber", "Analog", "Binary", "Brisk", "Copper", "Cosy", "Dusty", "Early",
		"Fuzzy", "Gentle", "Hollow", "Idle", "Jolly", "Lunar", "Mellow", "Misty",
		"Nimble", "Olive", "Plucky", "Polar", "Quiet", "Rusty", "Silver", "Sunny",
		"Tiny", "Upbeat", "Velvet", "Wired", "Woolly", "Young", "Zesty", "Kind",
	}
	callNoun = []string{
		"Badger", "Beacon", "Biscuit", "Cactus", "Compass", "Cursor", "Daemon", "Dongle",
		"Ferret", "Floppy", "Gecko", "Heron", "Kettle", "Lantern", "Mitten", "Modem",
		"Otter", "Pager", "Pebble", "Penguin", "Pigeon", "Printer", "Router", "Socket",
		"Spanner", "Stapler", "Teapot", "Toaster", "Turnip", "Walrus", "Widget", "Lamp",
	}
)

// callsign turns a key id into a friendly, stable, meaningless name.
func callsign(id string) string {
	b, err := hex.DecodeString(id)
	if err != nil || len(b) < 2 {
		return "Unknown Human"
	}
	return callAdj[int(b[0])%len(callAdj)] + " " + callNoun[int(b[1])%len(callNoun)]
}
