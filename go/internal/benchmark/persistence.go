// SPDX-License-Identifier: AGPL-3.0-or-later

package benchmark

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	stateName      = "jobs.json"
	maxFileBytes   = 1 << 20
	maxRecordBytes = 8192
)

type diskState struct {
	Version int      `json:"version"`
	Jobs    []record `json:"jobs"`
}

// Open exclusively opens <dataDir>/benchmark/jobs.json. dataDir must already
// exist; it and its ancestors are trusted operator-owned paths, never HTTP input.
// The dedicated directory is created 0700; files are 0600. Existing symlinks,
// nonregular files, permissive modes and invalid/oversize state are rejected.
// Restarts durably convert leased nonterminal jobs to recovery_required BEFORE
// returning, except prepared Apply decisions, which remain unchanged and fenced.
// Tokens are retained. Expired unleased jobs fail.
// An Open error MUST disable benchmark dispatch AND scheduler admission until
// an operator resolves storage/recovery; never substitute an empty store.
func Open(dataDir string) (*Store, error) {
	base, err := os.Lstat(dataDir)
	if err != nil || !base.IsDir() || base.Mode()&os.ModeSymlink != 0 {
		return nil, ErrStorage
	}
	dir := filepath.Join(dataDir, "benchmark")
	mkdirErr := os.Mkdir(dir, 0700)
	if mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
		return nil, ErrStorage
	}
	created := mkdirErr == nil
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return nil, ErrStorage
	}
	// Also sync on subsequent opens: a prior creation may have failed its sync.
	if err := syncDir(dataDir); err != nil {
		return nil, ErrStorage
	}
	lock, err := acquireLock(filepath.Join(dir, "writer.lock"))
	if err != nil {
		return nil, err
	}
	s := newStore(dir, lock)
	ok := false
	defer func() {
		if !ok {
			_ = lock.Close()
		}
	}()
	jobs, missing, err := load(filepath.Join(dir, stateName))
	if err != nil {
		return nil, err
	}
	// A vanished ledger is not a fresh installation. In particular, never
	// discard fences by silently replacing a missing existing state file.
	if missing && !created {
		return nil, ErrCorrupt
	}
	s.jobs = jobs
	s.index()
	changed := missing
	now := s.now()
	for id, r := range s.jobs {
		if next, recovered := expire(r, now, true); recovered {
			s.jobs[id] = next
			changed = true
		}
	}
	if changed {
		if err := s.commit(s.jobs); err != nil {
			return nil, err
		}
	}
	ok = true
	return s, nil
}

func encode(jobs map[string]record) ([]byte, error) {
	if len(jobs) > MaxJobs {
		return nil, ErrCorrupt
	}
	d := diskState{Version: SchemaVersion, Jobs: make([]record, 0, len(jobs))}
	active := make(map[string]bool)
	history := 0
	for id, r := range jobs {
		if id != r.Job.ID || !validRecord(r) {
			return nil, ErrCorrupt
		}
		if r.Job.State.Terminal() {
			history++
		} else {
			if active[r.Job.Machine] {
				return nil, ErrCorrupt
			}
			active[r.Job.Machine] = true
		}
		d.Jobs = append(d.Jobs, r)
	}
	if history > MaxHistory {
		return nil, ErrCorrupt
	}
	sort.Slice(d.Jobs, func(i, j int) bool { return d.Jobs[i].Job.ID < d.Jobs[j].Job.ID })
	b, err := json.Marshal(d)
	if err != nil || len(b) > maxFileBytes {
		return nil, ErrCorrupt
	}
	return b, nil
}

// privateFile rejects symlinks and nonregular or nonprivate files. The containing
// directory is private and trusted; mutation by the same OS owner is out of scope.
func privateFile(path string) (os.FileInfo, error) {
	i, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !i.Mode().IsRegular() || i.Mode().Perm() != 0600 {
		return nil, ErrStorage
	}
	return i, nil
}

func load(path string) (map[string]record, bool, error) {
	info, err := privateFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]record), true, nil
	}
	if err != nil {
		return nil, false, ErrStorage
	}
	if info.Size() <= 0 || info.Size() > maxFileBytes {
		return nil, false, ErrCorrupt
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false, ErrStorage
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, false, ErrStorage
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, false, ErrStorage
	}
	if len(b) > maxFileBytes {
		return nil, false, ErrCorrupt
	}
	jobs, err := decode(b)
	return jobs, false, err
}

// Decode the array incrementally: reject record 257 before allocating/decoding
// another Job. Input bytes, individual records, nesting and collection counts
// are all independently bounded. Unknown fields and duplicate keys are errors.
func decode(b []byte) (map[string]record, error) {
	if len(b) > maxFileBytes {
		return nil, ErrCorrupt
	}
	d := json.NewDecoder(bytes.NewReader(b))
	if !delim(d, '{') {
		return nil, ErrCorrupt
	}
	jobs := make(map[string]record)
	active := make(map[string]bool)
	seen := make(map[string]bool)
	history := 0
	for d.More() {
		t, err := d.Token()
		key, isString := t.(string)
		if err != nil || !isString || seen[key] {
			return nil, ErrCorrupt
		}
		seen[key] = true
		switch key {
		case "version":
			var version int
			if d.Decode(&version) != nil || version != SchemaVersion {
				return nil, ErrCorrupt
			}
		case "jobs":
			if !delim(d, '[') {
				return nil, ErrCorrupt
			}
			for d.More() {
				if len(jobs) >= MaxJobs {
					return nil, ErrCorrupt
				}
				var raw json.RawMessage
				if d.Decode(&raw) != nil || len(raw) > maxRecordBytes || !uniqueJSON(raw) {
					return nil, ErrCorrupt
				}
				rd := json.NewDecoder(bytes.NewReader(raw))
				rd.DisallowUnknownFields()
				var r record
				if rd.Decode(&r) != nil || !validRecord(r) {
					return nil, ErrCorrupt
				}
				if _, exists := jobs[r.Job.ID]; exists {
					return nil, ErrCorrupt
				}
				if r.Job.State.Terminal() {
					history++
					if history > MaxHistory {
						return nil, ErrCorrupt
					}
				} else {
					if active[r.Job.Machine] {
						return nil, ErrCorrupt
					}
					active[r.Job.Machine] = true
				}
				jobs[r.Job.ID] = r
			}
			if !delim(d, ']') {
				return nil, ErrCorrupt
			}
		default:
			return nil, ErrCorrupt
		}
	}
	if !delim(d, '}') || !seen["version"] || !seen["jobs"] {
		return nil, ErrCorrupt
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrCorrupt
	}
	return jobs, nil
}

func delim(d *json.Decoder, want json.Delim) bool {
	t, err := d.Token()
	return err == nil && t == want
}

func uniqueJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 8 {
			return false
		}
		t, err := d.Token()
		if err != nil {
			return false
		}
		switch t {
		case json.Delim('{'):
			seen := make(map[string]bool)
			for d.More() {
				k, err := d.Token()
				key, ok := k.(string)
				if err != nil || !ok || seen[key] {
					return false
				}
				// JSON's struct decoder otherwise accepts case-folded aliases.
				for _, c := range key {
					if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
						return false
					}
				}
				seen[key] = true
				if !value(depth + 1) {
					return false
				}
			}
			return delim(d, '}')
		case json.Delim('['):
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
			return delim(d, ']')
		default:
			_, isDelimiter := t.(json.Delim)
			return t != nil && !isDelimiter
		}
	}
	if !value(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

func validTime(t time.Time) bool { return !t.IsZero() && t.Year() >= 2000 && t.Year() <= 9999 }

func validRecord(r record) bool {
	if r.Job.Action == ActionApply {
		return validApplyRecord(r)
	}
	if r.Job.Action != "" || r.Job.Result != "" || r.Job.ApplyRequest != nil || r.Job.ApplyPhase != "" {
		return false
	}
	j, p := r.Job, r.Job.Provenance
	o, err := j.Options.Normalize()
	if err != nil || o != j.Options || !validHex(j.ID, 64) || !validMachine(j.Machine) ||
		!p.Audit.valid() || p.SchemaVersion != SchemaVersion || p.PolicyVersion != PolicyVersion || p.FixtureVersion != FixtureVersion ||
		p.WorkerID != j.Machine || p.ModelSHA256 != o.ModelSHA256 || p.BuildDigest != o.BuildDigest ||
		!validTime(p.CreatedAt) || !validTime(p.DeadlineAt) || !validTime(j.UpdatedAt) ||
		!p.DeadlineAt.Equal(p.CreatedAt.Add(time.Duration(o.BudgetSeconds)*time.Second)) ||
		j.UpdatedAt.Before(p.CreatedAt) || j.Sequence > MaxSequence {
		return false
	}
	leased := r.Lease != ""
	if leased && !validHex(r.Lease, 64) {
		return false
	}
	if j.State.Terminal() {
		if !validTime(j.FinishedAt) || !j.FinishedAt.Equal(j.UpdatedAt) || j.Progress != Done ||
			(leased && (!j.Restored || j.Sequence == 0)) ||
			(!leased && (j.Restored || j.Sequence != 0)) {
			return false
		}
	} else if j.Restored || !j.FinishedAt.IsZero() || j.Summary != nil || j.Sequence >= MaxSequence {
		return false
	}
	if j.State != Completed && j.Summary != nil {
		return false
	}
	switch j.State {
	case Queued:
		return !leased && j.Sequence == 0 && j.Progress == Pending && j.Failure == NoFailure && j.UpdatedAt.Equal(p.CreatedAt)
	case Running:
		return leased && stage(j.Progress) >= 1 && stage(j.Progress) <= 6 && j.Failure == NoFailure
	case Cancelling:
		return leased && j.Progress == Restoring && j.Failure == NoFailure
	case RecoveryRequired:
		return leased && j.Progress == Restoring && validFailure(j.Failure)
	case Completed:
		return leased && j.Failure == NoFailure && j.Summary != nil && j.Summary.valid(o) && j.UpdatedAt.Before(p.DeadlineAt)
	case Failed:
		return validFailure(j.Failure) && (leased || (j.Failure == FailureDeadline && !j.UpdatedAt.Before(p.DeadlineAt)))
	case Cancelled:
		return j.Failure == NoFailure
	}
	return false
}

func (s *Store) writeAtomic(data []byte) error {
	f, err := os.CreateTemp(s.dir, ".jobs-*")
	if err != nil {
		return ErrStorage
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return ErrStorage
	}
	if _, err := f.Write(data); err != nil {
		return ErrStorage
	}
	if err := f.Sync(); err != nil {
		return ErrStorage
	}
	if err := f.Close(); err != nil {
		return ErrStorage
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, stateName)); err != nil {
		return ErrStorage
	}
	if err := syncDir(s.dir); err != nil {
		return ErrStorage
	}
	return nil
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return ErrStorage
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return ErrStorage
	}
	if err := f.Close(); err != nil {
		return ErrStorage
	}
	return nil
}
