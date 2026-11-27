// Package crypt implements the rclone "crypt" on-disk format (file contents
// and file names) so that data written by Crypta is readable by stock rclone
// and vice versa. The format is documented at https://rclone.org/crypt/ and
// this implementation follows rclone's backend/crypt/cipher.go (MIT, see NOTICE).
package crypt

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/rfjakob/eme"
	"golang.org/x/crypto/nacl/secretbox"
	"golang.org/x/crypto/scrypt"
)

const (
	NonceSize     = 24
	HeaderSize    = 8 + NonceSize
	BlockData     = 64 * 1024
	BlockOverhead = secretbox.Overhead
	BlockSize     = BlockData + BlockOverhead
	magic         = "RCLONE\x00\x00"
)

var defaultSalt = []byte{0xA8, 0x0D, 0xF4, 0x3A, 0x8F, 0xBD, 0x03, 0x08, 0xA7, 0xCA, 0xB8, 0x3E, 0x58, 0x1F, 0x86, 0xB1}

var (
	ErrBadName   = errors.New("crypt: not an encrypted name")
	ErrBadHeader = errors.New("crypt: bad file header (not an rclone crypt file)")
	ErrBadBlock  = errors.New("crypt: failed to authenticate block (wrong password or corrupt data)")
)

// Options mirror the rclone crypt remote options that affect the format.
type Options struct {
	Password           string // plain text
	Salt               string // plain text "password2", may be empty
	FilenameEncryption string // "standard" (default) or "off"
	PlainDirNames      bool   // directory_name_encryption = false
	FilenameEncoding   string // "base32" (default) or "base64"
	Suffix             string // used when FilenameEncryption is "off"; default ".bin"
}

type Nonce [NonceSize]byte

func NewNonce() (n Nonce, err error) {
	_, err = io.ReadFull(rand.Reader, n[:])
	return
}

// Add returns n + x, treating n as a little endian number like rclone does.
func (n Nonce) Add(x uint64) Nonce {
	carry := uint16(0)
	for i := 0; i < 8; i++ {
		sum := uint16(n[i]) + uint16(byte(x)) + carry
		n[i] = byte(sum)
		carry = sum >> 8
		x >>= 8
	}
	for i := 8; i < NonceSize && carry != 0; i++ {
		sum := uint16(n[i]) + carry
		n[i] = byte(sum)
		carry = sum >> 8
	}
	return n
}

type Cipher struct {
	dataKey   [32]byte
	nameKey   [32]byte
	nameTweak [16]byte
	block     cipher.Block
	opt       Options
}

func New(opt Options) (*Cipher, error) {
	switch opt.FilenameEncryption {
	case "":
		opt.FilenameEncryption = "standard"
	case "standard", "off":
	default:
		return nil, fmt.Errorf("crypt: filename_encryption %q is not supported (use standard or off)", opt.FilenameEncryption)
	}
	switch opt.FilenameEncoding {
	case "":
		opt.FilenameEncoding = "base32"
	case "base32", "base64":
	default:
		return nil, fmt.Errorf("crypt: filename_encoding %q is not supported (use base32 or base64)", opt.FilenameEncoding)
	}
	if opt.Suffix == "" {
		opt.Suffix = ".bin"
	} else if opt.Suffix == "none" {
		opt.Suffix = ""
	}
	c := &Cipher{opt: opt}
	const keySize = 32 + 32 + 16
	salt := defaultSalt
	if opt.Salt != "" {
		salt = []byte(opt.Salt)
	}
	key := make([]byte, keySize)
	if opt.Password != "" {
		var err error
		key, err = scrypt.Key([]byte(opt.Password), salt, 16384, 8, 1, keySize)
		if err != nil {
			return nil, err
		}
	}
	copy(c.dataKey[:], key)
	copy(c.nameKey[:], key[32:])
	copy(c.nameTweak[:], key[64:])
	var err error
	c.block, err = aes.NewCipher(c.nameKey[:])
	return c, err
}

// ---- names ----

func (c *Cipher) encodeName(b []byte) string {
	if c.opt.FilenameEncoding == "base64" {
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return strings.ToLower(strings.TrimRight(base32.HexEncoding.EncodeToString(b), "="))
}

func (c *Cipher) decodeName(s string) ([]byte, error) {
	if c.opt.FilenameEncoding == "base64" {
		return base64.RawURLEncoding.DecodeString(s)
	}
	if strings.HasSuffix(s, "=") {
		return nil, ErrBadName
	}
	s = strings.ToUpper(s)
	if pad := (8 - len(s)%8) % 8; pad > 0 {
		s += strings.Repeat("=", pad)
	}
	return base32.HexEncoding.DecodeString(s)
}

func (c *Cipher) encryptSegment(name string) string {
	if name == "" {
		return ""
	}
	pad := aes.BlockSize - len(name)%aes.BlockSize
	padded := append([]byte(name), bytes.Repeat([]byte{byte(pad)}, pad)...)
	return c.encodeName(eme.Transform(c.block, c.nameTweak[:], padded, eme.DirectionEncrypt))
}

func (c *Cipher) decryptSegment(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	raw, err := c.decodeName(name)
	if err != nil || len(raw) == 0 || len(raw)%aes.BlockSize != 0 || len(raw) > 2048 {
		return "", ErrBadName
	}
	padded := eme.Transform(c.block, c.nameTweak[:], raw, eme.DirectionDecrypt)
	pad := int(padded[len(padded)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(padded) {
		return "", ErrBadName
	}
	for _, b := range padded[len(padded)-pad:] {
		if int(b) != pad {
			return "", ErrBadName
		}
	}
	return string(padded[:len(padded)-pad]), nil
}

// EncryptName encrypts a single path segment (no slashes).
func (c *Cipher) EncryptName(name string, isDir bool) string {
	if c.opt.FilenameEncryption == "off" {
		if isDir {
			return name
		}
		return name + c.opt.Suffix
	}
	if isDir && c.opt.PlainDirNames {
		return name
	}
	return c.encryptSegment(name)
}

// DecryptName decrypts a single path segment.
func (c *Cipher) DecryptName(name string, isDir bool) (string, error) {
	if c.opt.FilenameEncryption == "off" {
		if isDir {
			return name, nil
		}
		if c.opt.Suffix == "" {
			return name, nil
		}
		if !strings.HasSuffix(name, c.opt.Suffix) || len(name) == len(c.opt.Suffix) {
			return "", ErrBadName
		}
		return strings.TrimSuffix(name, c.opt.Suffix), nil
	}
	if isDir && c.opt.PlainDirNames {
		return name, nil
	}
	return c.decryptSegment(name)
}

// ---- sizes ----

func EncryptedSize(size int64) int64 {
	blocks, rem := size/BlockData, size%BlockData
	n := int64(HeaderSize) + blocks*BlockSize
	if rem != 0 {
		n += rem + BlockOverhead
	}
	return n
}

func DecryptedSize(size int64) (int64, error) {
	size -= HeaderSize
	if size < 0 {
		return 0, ErrBadHeader
	}
	blocks, rem := size/BlockSize, size%BlockSize
	n := blocks * BlockData
	if rem != 0 {
		rem -= BlockOverhead
		if rem <= 0 {
			return 0, ErrBadHeader
		}
	}
	return n + rem, nil
}

// ---- data ----

// EncryptAt fills p with the encrypted stream of src (size bytes of plain
// text, encrypted with nonce) starting at encrypted offset off. Being able to
// regenerate any part of the stream is what makes uploads resumable after the
// process has been killed. It returns io.EOF once off reaches the end.
func (c *Cipher) EncryptAt(src io.ReaderAt, size int64, nonce Nonce, off int64, p []byte) (int, error) {
	total := EncryptedSize(size)
	if off >= total {
		return 0, io.EOF
	}
	n := 0
	if off < HeaderSize {
		var hdr [HeaderSize]byte
		copy(hdr[:], magic)
		copy(hdr[8:], nonce[:])
		n = copy(p, hdr[off:])
		off += int64(n)
	}
	plain := make([]byte, BlockData)
	for n < len(p) && off < total {
		blk := (off - HeaderSize) / BlockSize
		skip := int((off - HeaderSize) % BlockSize)
		want := int64(BlockData)
		if rest := size - blk*BlockData; rest < want {
			want = rest
		}
		if _, err := src.ReadAt(plain[:want], blk*BlockData); err != nil && !(err == io.EOF) {
			return n, err
		}
		bn := nonce.Add(uint64(blk))
		sealed := secretbox.Seal(nil, plain[:want], (*[NonceSize]byte)(&bn), &c.dataKey)
		m := copy(p[n:], sealed[skip:])
		n += m
		off += int64(m)
	}
	return n, nil
}

// ParseHeader validates a file header and returns its nonce.
func ParseHeader(hdr []byte) (n Nonce, err error) {
	if len(hdr) < HeaderSize || string(hdr[:8]) != magic {
		return n, ErrBadHeader
	}
	copy(n[:], hdr[8:HeaderSize])
	return n, nil
}

// BlockOffset is the encrypted offset at which block blk starts.
func BlockOffset(blk int64) int64 { return HeaderSize + blk*BlockSize }

// Decrypter turns a stream of encrypted blocks (starting at a block
// boundary, without the header) into plain text.
type Decrypter struct {
	c     *Cipher
	in    io.Reader
	nonce Nonce
	buf   []byte
	out   []byte
	err   error
}

// NewDecrypter reads blocks from in, the first of which is block number blk.
func (c *Cipher) NewDecrypter(in io.Reader, nonce Nonce, blk int64) *Decrypter {
	return &Decrypter{c: c, in: in, nonce: nonce.Add(uint64(blk)), buf: make([]byte, BlockSize)}
}

func (d *Decrypter) Read(p []byte) (int, error) {
	for len(d.out) == 0 {
		if d.err != nil {
			return 0, d.err
		}
		n, err := io.ReadFull(d.in, d.buf)
		if err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
		if n > 0 {
			if n <= BlockOverhead {
				d.err = ErrBadBlock
				return 0, d.err
			}
			out, ok := secretbox.Open(nil, d.buf[:n], (*[NonceSize]byte)(&d.nonce), &d.c.dataKey)
			if !ok {
				d.err = ErrBadBlock
				return 0, d.err
			}
			d.out = out
			d.nonce = d.nonce.Add(1)
		}
		if err != nil {
			d.err = err
		}
	}
	n := copy(p, d.out)
	d.out = d.out[n:]
	return n, nil
}
