package crypt

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
)

// rclone "obscures" passwords in rclone.conf with AES-CTR under a fixed,
// public key (see rclone fs/config/obscure). This is not encryption; it only
// has to be undone to read a config file.
var obscureKey = []byte{
	0x9c, 0x93, 0x5b, 0x48, 0x73, 0x0a, 0x55, 0x4d,
	0x6b, 0xfd, 0x7c, 0x63, 0xc8, 0x86, 0xa9, 0x2b,
	0xd3, 0x90, 0x19, 0x8e, 0xb8, 0x12, 0x8a, 0xfb,
	0xf4, 0xde, 0x16, 0x2b, 0x8b, 0x95, 0xf6, 0x38,
}

// Reveal undoes rclone's password obscuring.
func Reveal(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(raw) < aes.BlockSize {
		return "", errors.New("crypt: value is not an rclone obscured password")
	}
	blk, _ := aes.NewCipher(obscureKey)
	out := make([]byte, len(raw)-aes.BlockSize)
	cipher.NewCTR(blk, raw[:aes.BlockSize]).XORKeyStream(out, raw[aes.BlockSize:])
	return string(out), nil
}
