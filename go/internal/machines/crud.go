package machines

import (
	"errors"
	"fmt"
	"strings"
)

// truthy mirrors Python truthiness for JSON-decoded values.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case map[string]any:
		return len(x) > 0
	case []any:
		return len(x) > 0
	}
	return true
}

func checkDigest(v any) (string, error) {
	s, ok := v.(string)
	if ok {
		s = strings.ToLower(s)
	}
	if !ok || !hexRe.MatchString(s) {
		return "", errors.New("secret_sha256 must be 64 hex characters")
	}
	return s, nil
}

func (m *Machines) checkUnique(wid, digest, pub string) error {
	for other, it := range m.items {
		if other == wid {
			continue
		}
		if it.SecretSHA256 == digest {
			return fmt.Errorf("secret already used by %s", other)
		}
		if pub != "" && it.TunnelPubkey == pub {
			return fmt.Errorf("tunnel key already used by %s", other)
		}
	}
	return nil
}

func (m *Machines) withItems(fn func(map[string]Machine)) map[string]Machine {
	next := make(map[string]Machine, len(m.items)+1)
	for k, v := range m.items {
		next[k] = v
	}
	fn(next)
	return next
}

// Add ports Machines.add. b is a JSON-decoded request body.
func (m *Machines) Add(b map[string]any) (Public, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable {
		return Public{}, ErrUnavailable
	}
	for k := range b {
		switch k {
		case "worker_id", "secret_sha256", "tunnel_pubkey", "meta", "enabled":
		default:
			return Public{}, errors.New("unknown field")
		}
	}
	wid, ok := b["worker_id"].(string)
	if !ok || !ValidID(wid) {
		return Public{}, errors.New("worker_id must match " + IDPattern)
	}
	digest, err := checkDigest(b["secret_sha256"])
	if err != nil {
		return Public{}, err
	}
	pub := ""
	if truthy(b["tunnel_pubkey"]) {
		if pub, err = ParsePubkey(b["tunnel_pubkey"]); err != nil {
			return Public{}, err
		}
	}
	rawMeta := b["meta"]
	if !truthy(rawMeta) {
		rawMeta = map[string]any{}
	}
	cm, err := CleanMeta(rawMeta)
	if err != nil {
		return Public{}, err
	}
	meta := map[string]string{}
	for k, v := range cm {
		if v != nil {
			meta[k] = v.(string)
		}
	}
	enabled := true
	if v, present := b["enabled"]; present {
		e, ok := v.(bool)
		if !ok {
			return Public{}, errors.New("enabled must be boolean")
		}
		enabled = e
	}
	if _, dup := m.items[wid]; dup {
		return Public{}, fmt.Errorf("%w: %s", ErrExists, wid)
	}
	if err := m.checkUnique(wid, digest, pub); err != nil {
		return Public{}, err
	}
	it := Machine{SecretSHA256: digest, Enabled: enabled, Added: m.now(), Meta: meta, TunnelPubkey: pub}
	if err := m.commit(m.withItems(func(n map[string]Machine) { n[wid] = it })); err != nil {
		return Public{}, err
	}
	return m.public(wid), nil
}

// Update ports Machines.update; meta values of nil/"" remove the key.
func (m *Machines) Update(wid string, b map[string]any) (Public, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable {
		return Public{}, ErrUnavailable
	}
	if len(b) == 0 {
		return Public{}, errors.New("nothing to update")
	}
	for k := range b {
		switch k {
		case "secret_sha256", "tunnel_pubkey", "meta", "enabled":
		default:
			return Public{}, errors.New("nothing to update")
		}
	}
	cur, ok := m.items[wid]
	if !ok {
		return Public{}, fmt.Errorf("%w: %s", ErrNotFound, wid)
	}
	it := cur.clone()
	if v, ok := b["enabled"]; ok {
		e, isBool := v.(bool)
		if !isBool {
			return Public{}, errors.New("enabled must be boolean")
		}
		it.Enabled = e
	}
	if v, ok := b["secret_sha256"]; ok {
		d, err := checkDigest(v)
		if err != nil {
			return Public{}, err
		}
		it.SecretSHA256 = d
	}
	if v, ok := b["tunnel_pubkey"]; ok {
		it.TunnelPubkey = ""
		if truthy(v) {
			p, err := ParsePubkey(v)
			if err != nil {
				return Public{}, err
			}
			it.TunnelPubkey = p
		}
	}
	if v, ok := b["meta"]; ok {
		cm, err := CleanMeta(v)
		if err != nil {
			return Public{}, err
		}
		for k, val := range cm {
			if val == nil {
				delete(it.Meta, k)
			} else {
				it.Meta[k] = val.(string)
			}
		}
	}
	if err := m.checkUnique(wid, it.SecretSHA256, it.TunnelPubkey); err != nil {
		return Public{}, err
	}
	if err := m.commit(m.withItems(func(n map[string]Machine) { n[wid] = it })); err != nil {
		return Public{}, err
	}
	return m.public(wid), nil
}

// Delete ports Machines.delete.
func (m *Machines) Delete(wid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable {
		return ErrUnavailable
	}
	if _, ok := m.items[wid]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, wid)
	}
	return m.commit(m.withItems(func(n map[string]Machine) { delete(n, wid) }))
}
