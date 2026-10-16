package machines

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrExists (Python KeyError) and ErrNotFound (Python LookupError).
var (
	ErrUnavailable = errors.New("machine registry unavailable; restart required")
	ErrExists      = errors.New("machine already exists")
	ErrNotFound    = errors.New("machine not found")
)

// Legacy holds the one-time migration inputs (all optional).
type Legacy struct {
	Secrets map[string]any            // worker_secrets.json: id -> digest
	Meta    map[string]map[string]any // config["macs"]
	Keys    map[string]string         // LegacyTunnelKeys(authorized_keys)
}

// Machines is the registry backed by <data>/machines.json.
type Machines struct {
	Path          string
	mu            sync.RWMutex
	items         map[string]Machine
	secretDigests map[string]bool
	secrets       map[string]string
	now           func() float64
	unavailable   bool
	failed        chan struct{}
	syncDir       func(string) error // per-registry durability fault seam
}

func nowFloat() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Open loads machines.json, or migrates from legacy data if it is absent.
func Open(data string, legacy Legacy) (*Machines, error) {
	m := &Machines{Path: filepath.Join(data, "machines.json"), now: nowFloat, failed: make(chan struct{}), syncDir: syncDirectory}
	raw, err := os.ReadFile(m.Path)
	switch {
	case err == nil:
		if m.items, err = decodeItems(raw); err != nil {
			return nil, err
		}
	case errors.Is(err, os.ErrNotExist):
		if err := m.migrate(legacy); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	m.index()
	return m, nil
}

func (m *Machines) migrate(l Legacy) error {
	var bad []string
	for w, d := range l.Secrets {
		ds, ok := d.(string)
		if !ValidID(w) || !ok || !hexRe.MatchString(ds) {
			bad = append(bad, w)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("machines: cannot migrate worker ids %s; rename them in worker_secrets.json (ids: %s, digests: 64 lowercase hex)", pyList(bad), IDPattern)
	}
	items := make(map[string]Machine, len(l.Secrets))
	for wid, d := range l.Secrets {
		filtered := map[string]any{}
		for k, v := range l.Meta[wid] {
			if isMetaKey(k) {
				filtered[k] = v
			}
		}
		meta := map[string]string{}
		if cm, err := CleanMeta(filtered); err == nil {
			for k, v := range cm {
				if v != nil {
					meta[k] = v.(string)
				}
			}
		}
		pub := ""
		if k := l.Keys[wid]; k != "" {
			if p, err := ParsePubkey(k); err == nil {
				pub = p
			}
		}
		items[wid] = Machine{SecretSHA256: d.(string), Enabled: true, Added: m.now(), Meta: meta, TunnelPubkey: pub}
	}
	if err := m.save(items); err != nil {
		return err
	}
	m.items = items
	return nil
}

func (m *Machines) index() {
	m.secrets = make(map[string]string, len(m.items))
	m.secretDigests = make(map[string]bool, len(m.items))
	for wid, it := range m.items {
		if it.Enabled {
			m.secrets[wid] = it.SecretSHA256
			m.secretDigests[it.SecretSHA256] = true
		}
	}
}

func (m *Machines) save(items map[string]Machine) error {
	s, err := encodeItems(items)
	if err != nil {
		return err
	}
	return writeAtomicSync(m.Path, s, m.syncDir)
}

// commit writes the candidate first and only then publishes it.
func (m *Machines) commit(items map[string]Machine) error {
	if m.unavailable {
		return ErrUnavailable
	}
	if err := m.save(items); err != nil {
		if errors.Is(err, ErrUnavailable) {
			m.unavailable = true
			close(m.failed)
		}
		return err
	}
	m.items = items
	m.index()
	return nil
}

func pyList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = "'" + strings.ReplaceAll(x, "'", `\'`) + "'"
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// Failed closes on uncertain persistence, waking waiting leases. Recovery
// requires reopening the registry; mutations cannot clear this state.
func (m *Machines) Failed() <-chan struct{} { return m.failed }

func (m *Machines) Available() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.unavailable
}

// Secret returns the digest for an enabled machine ("" if none).
func (m *Machines) Secret(wid string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.unavailable || !ValidID(wid) {
		return ""
	}
	return m.secrets[wid]
}

// CheckSecret mirrors app.py worker_auth: constant-time compare against the
// stored digest, or against 64 '0's when the id is unknown/disabled.
func (m *Machines) CheckSecret(wid, digest string) bool {
	want := m.Secret(wid)
	cmp := want
	if cmp == "" {
		cmp = strings.Repeat("0", 64)
	}
	ok := subtle.ConstantTimeCompare([]byte(digest), []byte(cmp)) == 1
	return want != "" && ok
}

// Meta returns a copy of the machine's metadata (empty if unknown).
func (m *Machines) Meta(wid string) map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]string{}
	for k, v := range m.items[wid].Meta {
		out[k] = v
	}
	return out
}

// Enabled reports whether wid exists and is enabled.
func (m *Machines) Enabled(wid string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	it, ok := m.items[wid]
	return !m.unavailable && ValidID(wid) && ok && it.Enabled
}

// Public is the admin-facing view of one machine (no secret, no raw key).
type Public struct {
	WorkerID             string            `json:"worker_id"`
	Enabled              bool              `json:"enabled"`
	Added                float64           `json:"added"`
	Meta                 map[string]string `json:"meta"`
	TunnelKeyFingerprint *string           `json:"tunnel_key_fingerprint"`
}

func (m *Machines) public(wid string) Public {
	it := m.items[wid]
	p := Public{WorkerID: wid, Enabled: it.Enabled, Added: it.Added, Meta: it.clone().Meta}
	if it.TunnelPubkey != "" {
		fp := Fingerprint(it.TunnelPubkey)
		p.TunnelKeyFingerprint = &fp
	}
	return p
}

// List returns all machines sorted by id.
func (m *Machines) List() []Public {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.items))
	for w := range m.items {
		ids = append(ids, w)
	}
	sort.Strings(ids)
	out := make([]Public, len(ids))
	for i, w := range ids {
		out[i] = m.public(w)
	}
	return out
}

// KnownSecret admits a bounded body only for an enabled registry credential.
// It does not replace the final worker-ID-bound check after parsing.
func (m *Machines) KnownSecret(digest string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.unavailable {
		return false, ErrUnavailable
	}
	return m.secretDigests[digest], nil
}
