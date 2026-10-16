package machines

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lucasfth/nocturne/go/internal/pyjson"
)

// Machine is one machines.json entry.
type Machine struct {
	SecretSHA256 string
	Enabled      bool
	Added        float64
	Meta         map[string]string
	TunnelPubkey string // "" means null
}

func (m Machine) clone() Machine {
	c := m
	c.Meta = make(map[string]string, len(m.Meta))
	for k, v := range m.Meta {
		c.Meta[k] = v
	}
	return c
}

func (m Machine) toJSON() map[string]any {
	meta := make(map[string]any, len(m.Meta))
	for k, v := range m.Meta {
		meta[k] = v
	}
	var pub any
	if m.TunnelPubkey != "" {
		pub = m.TunnelPubkey
	}
	return map[string]any{
		"secret_sha256": m.SecretSHA256, "enabled": m.Enabled,
		"added": m.Added, "meta": meta, "tunnel_pubkey": pub,
	}
}

type rawMachine struct {
	SecretSHA256 string            `json:"secret_sha256"`
	Enabled      *bool             `json:"enabled"`
	Added        float64           `json:"added"`
	Meta         map[string]string `json:"meta"`
	TunnelPubkey *string           `json:"tunnel_pubkey"`
}

func decodeItems(data []byte) (map[string]Machine, error) {
	var raw map[string]rawMachine
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("machines.json: %w", err)
	}
	items := make(map[string]Machine, len(raw))
	for wid, r := range raw {
		m := Machine{SecretSHA256: r.SecretSHA256, Enabled: r.Enabled == nil || *r.Enabled, Added: r.Added, Meta: r.Meta}
		if m.Meta == nil {
			m.Meta = map[string]string{}
		}
		if r.TunnelPubkey != nil {
			m.TunnelPubkey = *r.TunnelPubkey
		}
		items[wid] = m
	}
	return items, nil
}

// encodeItems reproduces json.dump(items, indent=1, sort_keys=True).
func encodeItems(items map[string]Machine) (string, error) {
	top := make(map[string]any, len(items))
	for wid, m := range items {
		top[wid] = m.toJSON()
	}
	return pyjson.MarshalIndentSorted(top, 1)
}

// writeAtomic: temp file (0600) in the same dir, fsync, rename, fsync dir.
func writeAtomic(path, content string) (err error) {
	return writeAtomicSync(path, content, syncDirectory)
}

func syncDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func writeAtomicSync(path, content string, syncDir func(string) error) (err error) {
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".machines.")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err = f.Chmod(0o600); err != nil {
		return err
	}
	if _, err = f.WriteString(content); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	if err = syncDir(dir); err != nil {
		// Replacement is visible but durability is unknown. Never expose the
		// underlying filesystem error (which may contain private paths).
		return ErrUnavailable
	}
	return nil
}
