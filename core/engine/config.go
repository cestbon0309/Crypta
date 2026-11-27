// Package engine joins the crypt format and a remote into a path based,
// decrypted file system plus a persistent, resumable transfer queue.
package engine

import (
	"bufio"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/cestbon0309/crypta/core/crypt"
	"github.com/cestbon0309/crypta/core/remote"
)

// Config is everything needed to reach the encrypted folder. It is what the
// iOS app keeps in the keychain and what pairing hands to a phone.
type Config struct {
	Backend   string             `json:"backend"` // "drive" or "local"
	Drive     remote.DriveConfig `json:"drive,omitempty"`
	LocalRoot string             `json:"localRoot,omitempty"`
	// Path is the folder inside the remote that holds the encrypted data
	// (the part after the colon in the crypt remote's "remote = gdrive:path").
	Path  string        `json:"path"`
	Crypt crypt.Options `json:"crypt"`
	// ChunkSize is the chunk_size of the rclone chunker remote on top of
	// the crypt remote; 0 means DefaultChunkSize.
	ChunkSize int64 `json:"chunkSize,omitempty"`
}

type iniSection map[string]string

func parseINI(text string) (map[string]iniSection, []string) {
	out := map[string]iniSection{}
	var order []string
	var cur iniSection
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' && line[len(line)-1] == ']' {
			name := line[1 : len(line)-1]
			cur = iniSection{}
			out[name] = cur
			order = append(order, name)
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && cur != nil {
			cur[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out, order
}

// CryptRemotes lists the crypt remotes of an rclone.conf that sit on Drive.
func CryptRemotes(conf string) []string {
	secs, order := parseINI(conf)
	var out []string
	for _, name := range order {
		if secs[name]["type"] == "crypt" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// FromRcloneConf builds a Config from the text of an rclone.conf. name picks
// the crypt remote; it may be empty when the file has exactly one.
func FromRcloneConf(conf, name string) (*Config, error) {
	secs, _ := parseINI(conf)
	if name == "" {
		names := CryptRemotes(conf)
		switch len(names) {
		case 0:
			return nil, fmt.Errorf("no crypt remote found in rclone.conf")
		case 1:
			name = names[0]
		default:
			return nil, fmt.Errorf("several crypt remotes found, choose one of: %s", strings.Join(names, ", "))
		}
	}
	name = strings.TrimSuffix(name, ":")
	cs, ok := secs[name]
	if !ok || cs["type"] != "crypt" {
		return nil, fmt.Errorf("rclone.conf has no crypt remote named %q", name)
	}
	if cs["password"] == "" {
		return nil, fmt.Errorf("crypt remote %q has no password; an encrypted rclone.conf must be decrypted first (rclone config show)", name)
	}
	baseName, path, ok := strings.Cut(cs["remote"], ":")
	if !ok {
		return nil, fmt.Errorf("crypt remote %q wraps a local path, expected a Google Drive remote", name)
	}
	ds, ok := secs[baseName]
	if !ok || ds["type"] != "drive" {
		return nil, fmt.Errorf("crypt remote %q must wrap a Google Drive remote directly (found %q of type %q)", name, baseName, ds["type"])
	}
	cfg := &Config{Backend: "drive", Path: strings.Trim(path, "/")}
	var err error
	if cfg.ChunkSize, err = chunkerOf(secs, name); err != nil {
		return nil, err
	}
	if cfg.Crypt.Password, err = crypt.Reveal(cs["password"]); err != nil {
		return nil, err
	}
	if cfg.Crypt.Salt, err = crypt.Reveal(cs["password2"]); err != nil {
		return nil, err
	}
	cfg.Crypt.FilenameEncryption = cs["filename_encryption"]
	cfg.Crypt.PlainDirNames = cs["directory_name_encryption"] == "false"
	cfg.Crypt.FilenameEncoding = cs["filename_encoding"]
	cfg.Crypt.Suffix = cs["suffix"]
	if ds["token"] == "" {
		return nil, fmt.Errorf("drive remote %q has no token; run `rclone config reconnect %s:` first", baseName, baseName)
	}
	cfg.Drive = remote.DriveConfig{ClientID: ds["client_id"], ClientSecret: ds["client_secret"], Token: json.RawMessage(ds["token"]),
		RootFolderID: ds["root_folder_id"], TeamDrive: ds["team_drive"]}
	if _, err := New(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
