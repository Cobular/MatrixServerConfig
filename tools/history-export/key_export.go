package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"

	"golang.org/x/crypto/pbkdf2"
)

const (
	exportPrefix           = "-----BEGIN MEGOLM SESSION DATA-----\n"
	exportSuffix           = "-----END MEGOLM SESSION DATA-----\n"
	exportVersion          = byte(1)
	exportRounds           = 100000
	exportHeaderLength     = 1 + 16 + 16 + 4
	exportHashLength       = 32
	exportLineLength       = 76
	generatedPassphraseLen = 32
)

type senderClaimedKeys struct {
	Ed25519 string `json:"ed25519"`
}

type exportedSession struct {
	Algorithm         string            `json:"algorithm"`
	ForwardingChains  []string          `json:"forwarding_curve25519_key_chain"`
	RoomID            string            `json:"room_id"`
	SenderKey         string            `json:"sender_key"`
	SenderClaimedKeys senderClaimedKeys `json:"sender_claimed_keys"`
	SessionID         string            `json:"session_id"`
	SessionKey        string            `json:"session_key"`
	SharedHistory     bool              `json:"m.shared_history,omitempty"`
}

func generatePassphrase() (string, error) {
	value := make([]byte, generatedPassphraseLen)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func encryptKeyExport(passphrase string, sessions []exportedSession) ([]byte, error) {
	if passphrase == "" {
		return nil, fmt.Errorf("passphrase is empty")
	}
	if len(sessions) == 0 {
		return nil, fmt.Errorf("no sessions to export")
	}
	plaintext, err := json.Marshal(sessions)
	if err != nil {
		return nil, err
	}

	salt := make([]byte, 16)
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	iv[7] &= 0xfe

	derived := pbkdf2.Key([]byte(passphrase), salt, exportRounds, 64, sha512.New)
	encryptionKey, hashKey := derived[:32], derived[32:]
	exportData := make([]byte, exportHeaderLength+len(plaintext)+exportHashLength)
	exportData[0] = exportVersion
	copy(exportData[1:17], salt)
	copy(exportData[17:33], iv)
	binary.BigEndian.PutUint32(exportData[33:37], exportRounds)

	block, err := aes.NewCipher(encryptionKey)
	if err != nil {
		return nil, err
	}
	dataEnd := len(exportData) - exportHashLength
	cipher.NewCTR(block, iv).XORKeyStream(exportData[exportHeaderLength:dataEnd], plaintext)
	mac := hmac.New(sha256.New, hashKey)
	mac.Write(exportData[:dataEnd])
	copy(exportData[dataEnd:], mac.Sum(nil))

	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(exportData)))
	base64.StdEncoding.Encode(encoded, exportData)
	var output bytes.Buffer
	output.WriteString(exportPrefix)
	for offset := 0; offset < len(encoded); offset += exportLineLength {
		end := min(offset+exportLineLength, len(encoded))
		output.Write(encoded[offset:end])
		output.WriteByte('\n')
	}
	output.WriteString(exportSuffix)
	return output.Bytes(), nil
}
