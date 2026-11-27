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
	"time"

	"github.com/cestbon0309/crypta/core/api"
)

const (
	CodeTTL     = 10 * time.Minute
	maxFailures = 10
	alphabet    = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O/1/I
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
	Failures int       `json:"failures"`
}

type Store struct {
	Path string
	mu   sync.Mutex
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
	s.mu.Lock()
	defer s.mu.Unlock()
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
	d.Failures = 0
	return string(code[:4]) + "-" + string(code[4:]), s.save(d)
}

var ErrBadCode = errors.New("pairing code is wrong or expired; run `crypta pair` again")

// Redeem exchanges a pairing code for a new device token.
func (s *Store) Redeem(code, name string) (token string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
			d.Failures = 0
			return token, s.save(d)
		}
	}
	// Too many wrong guesses void all outstanding codes.
	if d.Failures++; d.Failures >= maxFailures {
		d.Pairings, d.Failures = nil, 0
	}
	s.save(d)
	return "", ErrBadCode
}

func (s *Store) Valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := api.TokenHash(token)
	for _, dev := range s.load().Devices {
		if api.SameToken(dev.TokenHash, h) {
			return true
		}
	}
	return false
}

func (s *Store) Devices() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load().Devices
}

func (s *Store) Revoke(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.load()
	for i, dev := range d.Devices {
		if dev.ID == id {
			d.Devices = append(d.Devices[:i], d.Devices[i+1:]...)
			return s.save(d) == nil
		}
	}
	return false
}
