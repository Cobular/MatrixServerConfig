package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"

	"golang.org/x/crypto/pbkdf2"
)

func TestEncryptKeyExportRoundTrip(t *testing.T) {
	want := []exportedSession{{
		Algorithm:         "m.megolm.v1.aes-sha2",
		ForwardingChains:  []string{},
		RoomID:            "!room:example.com",
		SenderKey:         "sender",
		SenderClaimedKeys: senderClaimedKeys{Ed25519: "signing"},
		SessionID:         "session",
		SessionKey:        "key",
	}}
	data, err := encryptKeyExport("correct horse battery staple", want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decryptTestExport("correct horse battery staple", data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RoomID != want[0].RoomID || got[0].SessionKey != want[0].SessionKey {
		t.Fatalf("round-trip mismatch: %#v", got)
	}
}

func TestSharedHistoryField(t *testing.T) {
	without, err := json.Marshal(exportedSession{})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(without, []byte("shared_history")) {
		t.Fatalf("false shared history should be omitted: %s", without)
	}
	with, err := json.Marshal(exportedSession{SharedHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(with, []byte(`"m.shared_history":true`)) {
		t.Fatalf("shared history field missing: %s", with)
	}
}

func decryptTestExport(passphrase string, data []byte) ([]exportedSession, error) {
	body := strings.TrimSuffix(strings.TrimPrefix(string(data), exportPrefix), exportSuffix)
	body = strings.ReplaceAll(body, "\n", "")
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return nil, err
	}
	salt, iv := raw[1:17], raw[17:33]
	rounds := binary.BigEndian.Uint32(raw[33:37])
	dataEnd := len(raw) - exportHashLength
	derived := pbkdf2.Key([]byte(passphrase), salt, int(rounds), 64, sha512.New)
	mac := hmac.New(sha256.New, derived[32:])
	mac.Write(raw[:dataEnd])
	if !hmac.Equal(raw[dataEnd:], mac.Sum(nil)) {
		return nil, hmacMismatchError{}
	}
	block, err := aes.NewCipher(derived[:32])
	if err != nil {
		return nil, err
	}
	plaintext := make([]byte, dataEnd-exportHeaderLength)
	cipher.NewCTR(block, iv).XORKeyStream(plaintext, raw[exportHeaderLength:dataEnd])
	var sessions []exportedSession
	err = json.Unmarshal(plaintext, &sessions)
	return sessions, err
}

type hmacMismatchError struct{}

func (hmacMismatchError) Error() string { return "HMAC mismatch" }
