package engine

import (
	"crypto/rand"
	"crypto/sha1"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var (
	thumbNSOnce sync.Once
	thumbNS     [16]byte

	reLogURL     = regexp.MustCompile(`https?://[^\s"'<>]+`)
	reLogPikPak  = regexp.MustCompile(`pikpak:[^\s"'<>]+`)
	reLogWinPath = regexp.MustCompile(`[A-Za-z]:\\[^\s"':,)\]]+`)
	// Match absolute Unix paths preceded by start-of-string or a delimiter,
	// avoiding false positives inside tokens like "HTTP/1.1".
	reLogUnixPath = regexp.MustCompile(`(^|[\s"'(\[=:])(/(?:[^\s"':,)\]]+))`)
)

// newUUID returns a random RFC 4122 version 4 UUID.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b)
}

func formatUUID(b [16]byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// thumbLogID returns a deterministic, process-salted UUID v5 for a stored
// remote item so logs never reveal plaintext filenames or raw Drive IDs.
func thumbLogID(itemID string) string {
	if itemID == "" {
		return newUUID()
	}
	thumbNSOnce.Do(func() {
		if _, err := rand.Read(thumbNS[:]); err != nil {
			panic(err)
		}
	})
	h := sha1.New()
	h.Write(thumbNS[:])
	h.Write([]byte(itemID))
	sum := h.Sum(nil)
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x50 // version 5
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return formatUUID(b)
}

// formatSafeError formats err while stripping embedded URLs and OS paths
// from standard library error types before regex/substring redaction.
func formatSafeError(err error) string {
	if err == nil {
		return ""
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Sprintf("%s <url>: %s", ue.Op, formatSafeError(ue.Err))
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Sprintf("%s <path>: %s", pe.Op, formatSafeError(pe.Err))
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return fmt.Sprintf("%s <path> <path>: %s", le.Op, formatSafeError(le.Err))
	}
	return err.Error()
}

// redactError sanitizes err so that none of the sensitive strings (filenames,
// paths, URLs) or unrecognized file paths / URLs can leak into logs.
func redactError(err error, fileID string, sensitive ...string) string {
	if err == nil {
		return ""
	}
	return redactString(formatSafeError(err), fileID, sensitive...)
}

func redactString(s, fileID string, sensitive ...string) string {
	if s == "" {
		return ""
	}
	if fileID == "" {
		fileID = "<redacted>"
	}
	seen := map[string]bool{}
	var targets []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || v == "." || v == "/" || v == `\` || v == fileID || seen[v] {
			return
		}
		seen[v] = true
		targets = append(targets, v)
	}
	for _, raw := range sensitive {
		if raw == "" {
			continue
		}
		add(raw)
		slashed := filepath.ToSlash(raw)
		add(slashed)
		if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
			if u, err := url.Parse(raw); err == nil && u.Path != "" {
				add(u.Path)
				if base := path.Base(u.Path); base != "" && base != "." && base != "/" {
					add(base)
				}
				if unesc, err := url.PathUnescape(u.Path); err == nil && unesc != u.Path {
					add(unesc)
					if base := path.Base(unesc); base != "" && base != "." && base != "/" {
						add(base)
					}
				}
			}
		}
		if base := path.Base(slashed); base != "" && base != "." && base != "/" {
			add(base)
		}
		if dir := path.Dir(slashed); dir != "" && dir != "." && dir != "/" {
			add(dir)
		}
		if unesc, err := url.PathUnescape(raw); err == nil && unesc != raw {
			add(unesc)
			if base := path.Base(unesc); base != "" && base != "." && base != "/" {
				add(base)
			}
		}
		if esc := url.PathEscape(raw); esc != raw {
			add(esc)
		}
	}
	// Replace longer strings first so full paths are substituted before basenames.
	sort.Slice(targets, func(i, j int) bool { return len(targets[i]) > len(targets[j]) })
	out := s
	for _, tok := range targets {
		if len(tok) >= 4 || strings.ContainsAny(tok, `/\`) {
			out = strings.ReplaceAll(out, tok, fileID)
		} else {
			out = replaceBounded(out, tok, fileID)
		}
	}
	// Defense-in-depth: scrub any remaining URLs, PikPak paths, or filesystem paths.
	out = reLogURL.ReplaceAllString(out, "<url>")
	out = reLogPikPak.ReplaceAllString(out, "pikpak:<redacted>")
	out = reLogWinPath.ReplaceAllString(out, "<path>")
	out = reLogUnixPath.ReplaceAllStringFunc(out, func(m string) string {
		sub := reLogUnixPath.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		// Keep a single leading slash if nothing follows.
		if sub[2] == "/" {
			return m
		}
		return sub[1] + "<path>"
	})
	// Normalize line breaks to keep each log entry on a single line.
	out = strings.TrimSpace(strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(out))
	return out
}

// replaceBounded replaces short tokens only when surrounded by non-alphanumeric
// delimiters so short filenames like "a" or "1" do not mangle ordinary words.
func replaceBounded(s, tok, repl string) string {
	var b strings.Builder
	isWord := func(c byte) bool {
		return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
	}
	for i := 0; i < len(s); {
		idx := strings.Index(s[i:], tok)
		if idx < 0 {
			b.WriteString(s[i:])
			break
		}
		pos := i + idx
		end := pos + len(tok)
		leftOK := pos == 0 || !isWord(s[pos-1])
		rightOK := end == len(s) || !isWord(s[end])
		b.WriteString(s[i:pos])
		if leftOK && rightOK {
			b.WriteString(repl)
		} else {
			b.WriteString(tok)
		}
		i = end
	}
	return b.String()
}

func redactTransferError(m *Manager, t *Transfer, err error) string {
	if err == nil {
		return ""
	}
	// pikpakcli output can include arbitrary child filenames inside a folder;
	// map it to a fixed summary for logs rather than printing raw stdout.
	if t != nil && t.Kind == KindPikPak && strings.HasPrefix(err.Error(), "pikpakcli:") {
		low := strings.ToLower(err.Error())
		switch {
		case strings.Contains(low, "login"):
			return "pikpakcli: login failed"
		case strings.Contains(low, "did not finish"):
			return "pikpakcli: download incomplete"
		case strings.Contains(low, "nothing was downloaded"):
			return "pikpakcli: nothing downloaded"
		case strings.Contains(low, "resolve") || strings.Contains(low, "not found"):
			return "pikpakcli: target not found"
		default:
			return "pikpakcli: download failed"
		}
	}
	if t == nil {
		return redactError(err, "")
	}
	sens := []string{t.Remote, t.Name, t.Local, t.Source, t.URL}
	if m != nil && t.Local != "" {
		sens = append(sens, m.abs(t.Local))
	}
	return redactError(err, t.ID, sens...)
}

func redactItemError(it *Item, fileID string, err error) string {
	if it == nil {
		return redactError(err, fileID)
	}
	return redactError(err, fileID, it.Path, it.Name, it.id)
}
