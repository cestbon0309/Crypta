// Package auth keeps the server's pairing codes and device tokens in a JSON
// file that both the running server and the CLI (crypta pair) use.
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cestbon0309/crypta/core/api"
)

const (
	CodeTTL  = 10 * time.Minute
	alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O/1/I
)

type Pairing struct {
	CodeHash string    `json:"codeHash"`
	Expires  time.Time `json:"expires"`
}

type Device struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	TokenHash string    `json:"tokenHash"`
	Created   time.Time `json:"created"`
}

type data struct {
	Pairings []Pairing `json:"pairings"`
	Devices  []Device  `json:"devices"`
}

type Store struct {
	Path string
	mu   sync.Mutex
}

// lock also holds a file lock: the server and the CLI are separate
// processes that read, change and rewrite the same file.
func (s *Store) lock() func() {
	s.mu.Lock()
	f, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	}
	return func() {
		if f != nil {
			f.Close()
		}
		s.mu.Unlock()
	}
}

func (s *Store) load() data {
	var d data
	if b, err := os.ReadFile(s.Path); err == nil {
		json.Unmarshal(b, &d)
	}
	now := time.Now()
	live := d.Pairings[:0]
	for _, p := range d.Pairings {
		if p.Expires.After(now) {
			live = append(live, p)
		}
	}
	d.Pairings = live
	return d
}

func (s *Store) save(d data) error {
	b, _ := json.MarshalIndent(d, "", "  ")
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

func Random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func NormalizeCode(c string) string {
	c = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(c))
	return c
}

// NewCode creates a one-time pairing code like "ABCD-EFGH".
func (s *Store) NewCode() (string, error) {
	defer s.lock()()
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	code := make([]byte, 8)
	for i, b := range raw {
		code[i] = alphabet[int(b)%len(alphabet)]
	}
	d := s.load()
	d.Pairings = append(d.Pairings, Pairing{CodeHash: api.TokenHash(string(code)), Expires: time.Now().Add(CodeTTL)})
	return string(code[:4]) + "-" + string(code[4:]), s.save(d)
}

var ErrBadCode = errors.New("pairing code is wrong or expired; run `crypta pair` again")

// Redeem exchanges a pairing code for a new device token.
func (s *Store) Redeem(code, name string) (token string, err error) {
	defer s.lock()()
	d := s.load()
	h := api.TokenHash(NormalizeCode(code))
	for i, p := range d.Pairings {
		if api.SameToken(p.CodeHash, h) {
			d.Pairings = append(d.Pairings[:i], d.Pairings[i+1:]...)
			token = Random(32)
			if name == "" {
				name = "iPhone"
			}
			d.Devices = append(d.Devices, Device{ID: Random(4), Name: name, TokenHash: api.TokenHash(token), Created: time.Now()})
			return token, s.save(d)
		}
	}
	// A code has 40 random bits and lives ten minutes, so guessing is
	// hopeless; wrong guesses must not cost the owner the real code.
	return "", ErrBadCode
}

func (s *Store) Valid(token string) bool {
	if token == "" {
		return false
	}
	defer s.lock()()
	h := api.TokenHash(token)
	for _, dev := range s.load().Devices {
		if api.SameToken(dev.TokenHash, h) {
			return true
		}
	}
	return false
}

func (s *Store) Devices() []Device {
	defer s.lock()()
	return s.load().Devices
}

func (s *Store) Revoke(id string) bool {
	defer s.lock()()
	d := s.load()
	for i, dev := range d.Devices {
		if dev.ID == id {
			d.Devices = append(d.Devices[:i], d.Devices[i+1:]...)
			return s.save(d) == nil
		}
	}
	return false
}
