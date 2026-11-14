package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Stale-read gate
//
// An edit anchor is copied text, not a file handle. Between read_file and
// replace_lines the file can change under the model: the user saves it in
// their editor, a formatter rewrites it through run_bash, a code generator
// regenerates it. The anchor gate only answers "does this text occur
// exactly once", so when the same block still occurs once — after a shift,
// or inside a duplicated helper, or in a file that grew a second copy — the
// edit lands in the wrong place and every downstream check still passes,
// because the result compiles.
//
// So the sandbox remembers the digest of every file the model has observed
// — a read_file result, or the content its own write_file produced — and
// refuses a patch whose target no longer matches. The refusal tells the
// model to read the file again, which is the same recovery a human would
// perform. Whole-file writes are not gated: write_file states the entire
// result, so there is no stale anchor to misplace.
//
// The record is keyed by workspace-relative path, not absolute path, so it
// survives a mirror swap (direct mode with manual approval works in a copy)
// and can be persisted into a conversation transcript.

const (
	// staleReadPrefix tags the refusal so the loop, the logs and the tests
	// recognize it without matching on prose.
	staleReadPrefix = "[stale-read]"
	// maxStaleDigestBytes bounds what we track. Past this size read_file
	// truncates anyway and hashing a generated monster on every read costs
	// more than the check is worth; such files are simply untracked, which
	// allows the edit rather than inventing a refusal.
	maxStaleDigestBytes = 32 << 20
)

// StaleReadError reports that a patch targeted a file whose content changed
// after the model last observed it. The edit was not applied.
type StaleReadError struct {
	// Path is workspace-relative so the message matches what the model
	// typed.
	Path string
	// Was is the digest of the content the model observed; Now is the
	// digest on disk when the edit arrived. Both are truncated in Error.
	Was string
	Now string
}

func (e *StaleReadError) Error() string {
	return fmt.Sprintf("%s %s changed on disk after you last read it (was %s, now %s); your edit was NOT applied. "+
		"Call read_file on %s again and redo the edit against the current content.",
		staleReadPrefix, e.Path, shortDigest(e.Was), shortDigest(e.Now), e.Path)
}

// digestBytes is the hex sha256 of b.
func digestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// shortDigest keeps the refusal readable: eight hex characters identify a
// version for a human without inviting the model to reason about them.
func shortDigest(d string) string {
	const n = 8
	if len(d) > n {
		return d[:n]
	}
	if d == "" {
		return "unknown"
	}
	return d
}

// staleKey normalizes a workspace-relative path to the map key form:
// slash-separated, cleaned, and empty for the workspace root.
func staleKey(rel string) string {
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || rel == "/" || rel == "" {
		return ""
	}
	return rel
}

// observeFile records the digest of the file the model just saw. Best
// effort: an unreadable or oversized file is left untracked, which allows
// later edits instead of blocking them on a check we cannot perform.
func (s *Sandbox) observeFile(abs, rel string) {
	if info, err := os.Stat(abs); err != nil || info.Size() > maxStaleDigestBytes {
		return
	}
	digest, err := fileDigest(abs)
	if err != nil || digest == "" {
		return
	}
	s.rememberObserved(rel, digest)
}

// rememberObserved records digest as the content the model knows for rel.
func (s *Sandbox) rememberObserved(rel, digest string) {
	key := staleKey(rel)
	if key == "" || digest == "" {
		return
	}
	s.observedMu.Lock()
	defer s.observedMu.Unlock()
	if s.observed == nil {
		s.observed = make(map[string]string)
	}
	s.observed[key] = digest
}

// checkStaleEdit refuses a patch when the model has observed a different
// content for rel. Files it never observed are allowed on purpose: the
// model may patch a file it just created, and a read from a previous run
// is not tracked here at all. The gate must never become a wall that
// blocks edits the engine cannot reason about.
func (s *Sandbox) checkStaleEdit(rel, current string) error {
	key := staleKey(rel)
	if key == "" || current == "" {
		return nil
	}
	s.observedMu.Lock()
	was, ok := s.observed[key]
	s.observedMu.Unlock()
	if !ok || was == current {
		return nil
	}
	return &StaleReadError{Path: key, Was: was, Now: current}
}

// forgetObserved drops the record for a deleted path (and, for a directory,
// everything beneath it).
func (s *Sandbox) forgetObserved(rel string) {
	key := staleKey(rel)
	s.observedMu.Lock()
	defer s.observedMu.Unlock()
	if key == "" {
		s.observed = nil
		return
	}
	delete(s.observed, key)
	prefix := key + "/"
	for k := range s.observed {
		if strings.HasPrefix(k, prefix) {
			delete(s.observed, k)
		}
	}
}

// renameObserved follows a move so the moved file keeps its record and the
// old path stops being tracked.
func (s *Sandbox) renameObserved(from, to string) {
	src, dst := staleKey(from), staleKey(to)
	if src == "" || dst == "" || src == dst {
		return
	}
	s.observedMu.Lock()
	defer s.observedMu.Unlock()
	moved := make(map[string]string, len(s.observed))
	for k, v := range s.observed {
		switch {
		case k == src:
			moved[dst] = v
			delete(s.observed, k)
		case strings.HasPrefix(k, src+"/"):
			moved[dst+k[len(src):]] = v
			delete(s.observed, k)
		}
	}
	for k, v := range moved {
		s.observed[k] = v
	}
}

// ObservedFiles returns a copy of the observed digests keyed by
// workspace-relative path. The direct loop persists this so a follow-up
// turn still refuses an edit written against a read from the previous turn.
func (s *Sandbox) ObservedFiles() map[string]string {
	s.observedMu.Lock()
	defer s.observedMu.Unlock()
	if len(s.observed) == 0 {
		return nil
	}
	out := make(map[string]string, len(s.observed))
	for k, v := range s.observed {
		out[k] = v
	}
	return out
}

// RestoreObserved seeds the gate from a snapshot produced by
// ObservedFiles. Existing entries win, so a restore cannot roll back a
// newer observation.
func (s *Sandbox) RestoreObserved(files map[string]string) {
	for k, v := range files {
		key := staleKey(k)
		if key == "" || v == "" {
			continue
		}
		s.observedMu.Lock()
		if s.observed == nil {
			s.observed = make(map[string]string)
		}
		if _, exists := s.observed[key]; !exists {
			s.observed[key] = v
		}
		s.observedMu.Unlock()
	}
}
