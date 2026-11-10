package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/esrrhs/divvy/pkg/tools"
)

// Limits for @-referenced context and per-turn change snapshots.
const (
	ctxMaxPaths        = 16
	ctxMaxFiles        = 32
	ctxMaxFileBytes    = 64 << 10 // 64 KiB of text included per file
	ctxMaxTotalBytes   = 256 << 10
	ctxHardFileBytes   = 1 << 20 // refuse to open files larger than 1 MiB
	ctxMaxDirList      = 40
	ctxSearchLimit     = 40
	snapMaxFiles       = 5000
	snapContentMax     = 64 << 10
	snapTotalContent   = 2 << 20
	turnMaxChangeFiles = 80
)

// fileRef is one resolved attachment shown back to the client.
type fileRef struct {
	Path      string `json:"path"`
	Type      string `json:"type"` // file | dir | selection
	Bytes     int    `json:"bytes,omitempty"`
	Excluded  string `json:"excluded,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	Draft     bool   `json:"draft,omitempty"`
}

type snapFile struct {
	Rel     string
	Size    int64
	Hash    string
	Binary  bool
	Content string // only when !Binary and Size <= snapContentMax
	Mode    uint32 // permission bits; 0 if unknown
}

// turnChange is the persisted, reviewable diff for one path in a turn.
type turnChange struct {
	Path          string   `json:"path"`
	Status        string   `json:"status"`
	Binary        bool     `json:"binary,omitempty"`
	Truncated     bool     `json:"truncated,omitempty"`
	Old           string   `json:"old_content,omitempty"`
	New           string   `json:"new_content,omitempty"`
	HunkDecisions []string `json:"hunk_decisions,omitempty"`
	Applied       string   `json:"applied_content,omitempty"`
	AppliedStatus string   `json:"applied_status,omitempty"`
	// OldMode/NewMode are Unix permission bits from the workdir snapshot.
	// Zero means the snapshot did not record a mode (legacy records).
	OldMode uint32 `json:"old_mode,omitempty"`
	NewMode uint32 `json:"new_mode,omitempty"`
}

// turnChangeSummary is the compact card payload stored on the message.
type turnChangeSummary struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Binary    bool   `json:"binary,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

func resolveProjectPath(workdir, userPath string) (abs, rel string, err error) {
	userPath = strings.TrimSpace(userPath)
	if userPath == "" {
		return "", "", fmt.Errorf("path is required")
	}
	if strings.Contains(userPath, "\x00") {
		return "", "", fmt.Errorf("invalid path")
	}
	sb, err := tools.NewSandbox(workdir)
	if err != nil {
		return "", "", err
	}
	// Allow an absolute path only when it still resolves inside the project.
	if filepath.IsAbs(userPath) {
		relTry, rerr := filepath.Rel(sb.Root, filepath.Clean(userPath))
		if rerr != nil || strings.HasPrefix(relTry, "..") {
			return "", "", fmt.Errorf("path escapes project directory")
		}
		userPath = relTry
	}
	resolved, err := sb.Resolve(userPath)
	if err != nil {
		return "", "", err
	}
	real, err := confinedRealPath(sb.Root, resolved)
	if err != nil {
		return "", "", fmt.Errorf("path escapes project directory")
	}
	rel, err = filepath.Rel(sb.Root, real)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", "", fmt.Errorf("path escapes project directory")
	}
	return real, filepath.ToSlash(rel), nil
}

// collectContext reads referenced files/dirs inside workdir and returns a
// prompt block plus the refs actually used. Secrets are not attached here;
// callers redact the block before persistence.
func collectContext(workdir string, paths []string) (block string, refs []fileRef, err error) {
	if len(paths) == 0 {
		return "", nil, nil
	}
	if len(paths) > ctxMaxPaths {
		return "", nil, fmt.Errorf("too many referenced paths (max %d)", ctxMaxPaths)
	}
	var b strings.Builder
	b.WriteString("\n\n---\nReferenced project files for this turn (read-only context; stay inside the workspace). Files on disk remain the source of truth:\n")
	total := 0
	filesUsed := 0
	seen := map[string]bool{}
	for _, raw := range paths {
		abs, rel, rerr := resolveProjectPath(workdir, raw)
		if rerr != nil {
			refs = append(refs, fileRef{Path: raw, Excluded: rerr.Error()})
			continue
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		info, lerr := os.Lstat(abs)
		if lerr != nil {
			refs = append(refs, fileRef{Path: rel, Excluded: "not found"})
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// resolveProjectPath already confined the target.
			if st, err := os.Stat(abs); err == nil {
				info = st
			}
		}
		if info.IsDir() {
			dirBlock, dirRefs, nfiles, nbytes := collectDirContext(abs, rel, ctxMaxFiles-filesUsed, ctxMaxTotalBytes-total)
			b.WriteString(dirBlock)
			refs = append(refs, dirRefs...)
			filesUsed += nfiles
			total += nbytes
			continue
		}
		if filesUsed >= ctxMaxFiles || total >= ctxMaxTotalBytes {
			refs = append(refs, fileRef{Path: rel, Type: "file", Excluded: "context budget exhausted"})
			continue
		}
		chunk, ref := readContextFile(abs, rel, ctxMaxTotalBytes-total)
		b.WriteString(chunk)
		refs = append(refs, ref)
		if ref.Excluded == "" {
			filesUsed++
			total += ref.Bytes
		}
	}
	return b.String(), refs, nil
}

func collectDirContext(abs, rel string, fileBudget, byteBudget int) (string, []fileRef, int, int) {
	var b strings.Builder
	fmt.Fprintf(&b, "\n### %s/\n", rel)
	entries, err := os.ReadDir(abs)
	if err != nil {
		fmt.Fprintf(&b, "(cannot list directory: %s)\n", err.Error())
		return b.String(), []fileRef{{Path: rel, Type: "dir", Excluded: err.Error()}}, 0, 0
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if tools.IsSkippedName(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	listed := names
	truncatedList := false
	if len(listed) > ctxMaxDirList {
		listed = listed[:ctxMaxDirList]
		truncatedList = true
	}
	fmt.Fprintf(&b, "Directory listing (%d entries%s):\n", len(names), tern(truncatedList, ", truncated", ""))
	for _, name := range listed {
		p := filepath.Join(abs, name)
		label := name
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			label += "/"
		}
		fmt.Fprintf(&b, "  %s\n", label)
	}

	refs := []fileRef{{Path: rel, Type: "dir"}}
	files, bytes := 0, 0
	for _, name := range names {
		if files >= fileBudget || bytes >= byteBudget {
			break
		}
		p := filepath.Join(abs, name)
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		childRel := rel + "/" + name
		chunk, ref := readContextFile(p, childRel, byteBudget-bytes)
		if ref.Excluded != "" {
			continue
		}
		b.WriteString(chunk)
		refs = append(refs, ref)
		files++
		bytes += ref.Bytes
	}
	return b.String(), refs, files, bytes
}

func readContextFile(abs, rel string, remain int) (string, fileRef) {
	info, err := os.Stat(abs)
	if err != nil {
		return "", fileRef{Path: rel, Type: "file", Excluded: "not found"}
	}
	if info.Size() > ctxHardFileBytes {
		return fmt.Sprintf("\n### %s\n(omitted: file is %d bytes, limit %d)\n", rel, info.Size(), ctxHardFileBytes),
			fileRef{Path: rel, Type: "file", Bytes: int(info.Size()), Excluded: "too large"}
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fileRef{Path: rel, Type: "file", Excluded: err.Error()}
	}
	if isBinarySniff(data) {
		return fmt.Sprintf("\n### %s\n(omitted: binary file, %d bytes)\n", rel, len(data)),
			fileRef{Path: rel, Type: "file", Bytes: len(data), Excluded: "binary"}
	}
	trunc := false
	content := data
	limit := ctxMaxFileBytes
	if remain > 0 && remain < limit {
		limit = remain
	}
	if len(content) > limit {
		content = content[:limit]
		trunc = true
		content = cutUTF8(content)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n### %s\n", rel)
	if trunc {
		fmt.Fprintf(&b, "(truncated to %d bytes)\n", len(content))
	}
	b.WriteString("```\n")
	b.Write(content)
	if !strings.HasSuffix(string(content), "\n") {
		b.WriteByte('\n')
	}
	b.WriteString("```\n")
	return b.String(), fileRef{Path: rel, Type: "file", Bytes: len(content), Truncated: trunc}
}

func cutUTF8(b []byte) []byte {
	for !utf8.Valid(b) && len(b) > 0 {
		b = b[:len(b)-1]
	}
	return b
}

func tern(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func searchProjectFiles(workdir, query string, limit int) ([]fileRef, error) {
	if limit <= 0 || limit > ctxSearchLimit {
		limit = ctxSearchLimit
	}
	query = strings.ToLower(strings.TrimSpace(query))
	sb, err := tools.NewSandbox(workdir)
	if err != nil {
		return nil, err
	}
	root := sb.Root
	var out []fileRef
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if path != root && tools.IsSkippedName(name) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil || strings.HasPrefix(rel, "..") {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if query != "" && !strings.Contains(strings.ToLower(relSlash), query) && !strings.Contains(strings.ToLower(name), query) {
			if d.IsDir() {
				return nil
			}
			return nil
		}
		typ := "file"
		if d.IsDir() {
			typ = "dir"
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		out = append(out, fileRef{Path: relSlash, Type: typ})
		if len(out) >= limit {
			return fs.SkipAll
		}
		return nil
	})
	return out, err
}

func snapshotWorkdir(workdir string) ([]snapFile, error) {
	sb, err := tools.NewSandbox(workdir)
	if err != nil {
		return nil, err
	}
	root := sb.Root
	var out []snapFile
	var totalContent int
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path != root && tools.IsSkippedName(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if len(out) >= snapMaxFiles {
			return fs.SkipAll
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil || strings.HasPrefix(rel, "..") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		sf := snapFile{Rel: filepath.ToSlash(rel), Size: info.Size(), Mode: uint32(info.Mode().Perm())}
		f, oerr := os.Open(path)
		if oerr != nil {
			return nil
		}
		head := make([]byte, 8*1024)
		n, _ := io.ReadFull(f, head)
		head = head[:max(0, n)]
		sf.Binary = isBinarySniff(head)
		_, _ = f.Seek(0, 0)
		sum := sha256.New()
		var buf []byte
		if !sf.Binary && sf.Size <= snapContentMax && totalContent+int(sf.Size) <= snapTotalContent {
			all, rerr := io.ReadAll(io.LimitReader(f, snapContentMax+1))
			if rerr == nil {
				buf = all
				_, _ = sum.Write(all)
			}
		} else {
			_, _ = io.Copy(sum, f)
		}
		_ = f.Close()
		sf.Hash = hex.EncodeToString(sum.Sum(nil))
		if buf != nil {
			sf.Content = string(buf)
			totalContent += len(buf)
		}
		out = append(out, sf)
		return nil
	})
	return out, err
}

func diffAgainstSnapshot(before []snapFile, workdir string) ([]turnChange, error) {
	after, err := snapshotWorkdir(workdir)
	if err != nil {
		return nil, err
	}
	prev := map[string]snapFile{}
	for _, s := range before {
		prev[s.Rel] = s
	}
	seen := map[string]bool{}
	var changes []turnChange
	for _, a := range after {
		seen[a.Rel] = true
		p, ok := prev[a.Rel]
		if !ok {
			changes = append(changes, turnChange{
				Path: a.Rel, Status: "added", Binary: a.Binary,
				Truncated: a.Binary || a.Content == "" && a.Size > 0,
				New:       a.Content,
				NewMode:   a.Mode,
			})
			continue
		}
		if p.Hash == a.Hash && p.Size == a.Size {
			// Both sides must have recorded a mode; a zero mode is a
			// legacy snapshot and cannot be treated as a recoverable
			// permission-only change.
			if p.Mode != 0 && a.Mode != 0 && p.Mode != a.Mode {
				changes = append(changes, turnChange{
					Path: a.Rel, Status: "modified", Binary: a.Binary || p.Binary,
					Truncated: a.Binary || p.Binary || (p.Content == "" && p.Size > 0) || (a.Content == "" && a.Size > 0),
					Old:       p.Content, New: a.Content,
					OldMode: p.Mode, NewMode: a.Mode,
				})
			}
			continue
		}
		changes = append(changes, turnChange{
			Path: a.Rel, Status: "modified", Binary: a.Binary || p.Binary,
			Truncated: a.Binary || p.Binary || (p.Content == "" && p.Size > 0) || (a.Content == "" && a.Size > 0),
			Old:       p.Content, New: a.Content,
			OldMode: p.Mode, NewMode: a.Mode,
		})
	}
	for _, p := range before {
		if seen[p.Rel] {
			continue
		}
		changes = append(changes, turnChange{
			Path: p.Rel, Status: "deleted", Binary: p.Binary,
			Truncated: p.Binary || (p.Content == "" && p.Size > 0),
			Old:       p.Content,
			OldMode:   p.Mode,
		})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	if len(changes) > turnMaxChangeFiles {
		changes = changes[:turnMaxChangeFiles]
	}
	return changes, nil
}

func changesFilePath(dataDir, convID, sessionID string) string {
	return filepath.Join(dataDir, "conversations", "changes", convID, sessionID+".json")
}

func saveTurnChanges(dataDir, convID, sessionID string, changes []turnChange, secrets []string) error {
	if !validConversationID(convID) || !toolsValidSessionish(sessionID) {
		return fmt.Errorf("invalid id")
	}
	for i := range changes {
		changes[i].Old = Redact(changes[i].Old, secrets...)
		changes[i].New = Redact(changes[i].New, secrets...)
	}
	dir := filepath.Dir(changesFilePath(dataDir, convID, sessionID))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(changes, "", "  ")
	if err != nil {
		return err
	}
	data = RedactBytes(data, secrets...)
	tmp, err := os.CreateTemp(dir, "chg_*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, changesFilePath(dataDir, convID, sessionID))
}

func loadTurnChanges(dataDir, convID, sessionID string) ([]turnChange, error) {
	if !validConversationID(convID) || !toolsValidSessionish(sessionID) {
		return nil, fmt.Errorf("invalid id")
	}
	data, err := os.ReadFile(changesFilePath(dataDir, convID, sessionID))
	if err != nil {
		return nil, err
	}
	var changes []turnChange
	if err := json.Unmarshal(data, &changes); err != nil {
		return nil, err
	}
	return changes, nil
}

func summarizeChanges(changes []turnChange) []turnChangeSummary {
	out := make([]turnChangeSummary, 0, len(changes))
	for _, c := range changes {
		out = append(out, turnChangeSummary{Path: c.Path, Status: c.Status, Binary: c.Binary, Truncated: c.Truncated})
	}
	return out
}

func toolsValidSessionish(id string) bool {
	if id == "" || len(id) > 128 || strings.Contains(id, "..") || strings.ContainsAny(id, "/\\") {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// Auto-context budgets: a project map and a few related snippets, never the
// whole repository verbatim.
const (
	autoMapMaxEntries  = 200
	autoMapMaxBytes    = 8 << 10
	autoRecentMaxBytes = 2 << 10
	autoHitMax         = 10
	autoHitBytes       = 8 << 10
	autoTotalMaxBytes  = 16 << 10
	autoGrepFileMax    = 64 << 10
)

// collectProjectContext builds a bounded, automatic snapshot of the project
// so a turn does not depend on the user @-mentioning files. Explicit
// attachments are added separately and are not replaced by this.
func collectProjectContext(workdir, userText string) string {
	if strings.TrimSpace(workdir) == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n---\nAutomatic project context (bounded map / recent changes / keyword hits; not the full repo; @ attachments are added separately):\n")
	used := 0
	if m := projectFileMap(workdir); m != "" {
		b.WriteString(m)
		used += len(m)
	}
	if r := projectRecentChanges(workdir); r != "" && used+len(r) < autoTotalMaxBytes {
		b.WriteString(r)
		used += len(r)
	}
	if h := projectKeywordHits(workdir, userText); h != "" && used+len(h) < autoTotalMaxBytes {
		remain := autoTotalMaxBytes - used
		if len(h) > remain {
			h = h[:remain] + "\n…(related-file hits truncated)\n"
		}
		b.WriteString(h)
	}
	out := b.String()
	if len(out) > autoTotalMaxBytes+256 {
		out = out[:autoTotalMaxBytes] + "\n…(automatic context truncated)\n"
	}
	return out
}

func projectFileMap(workdir string) string {
	sb, err := tools.NewSandbox(workdir)
	if err != nil {
		return ""
	}
	root := sb.Root
	var paths []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == root {
			return nil
		}
		name := d.Name()
		if tools.IsSkippedName(name) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			paths = append(paths, rel+"/")
		} else {
			paths = append(paths, rel)
		}
		if len(paths) >= autoMapMaxEntries {
			return fs.SkipAll
		}
		return nil
	})
	if len(paths) == 0 {
		return "Project map: (empty workdir)\n"
	}
	sort.Strings(paths)
	var b strings.Builder
	fmt.Fprintf(&b, "Project map (%d paths", len(paths))
	if len(paths) >= autoMapMaxEntries {
		b.WriteString(", truncated")
	}
	b.WriteString("):\n")
	for _, p := range paths {
		line := "  " + p + "\n"
		if b.Len()+len(line) > autoMapMaxBytes {
			b.WriteString("  …(map truncated)\n")
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

func projectRecentChanges(workdir string) string {
	gitDir := filepath.Join(workdir, ".git")
	if st, err := os.Stat(gitDir); err == nil && st.IsDir() {
		var b strings.Builder
		b.WriteString("Recent git changes:\n")
		if s := gitOut(workdir, "status", "--short"); s != "" {
			b.WriteString(s)
			if !strings.HasSuffix(s, "\n") {
				b.WriteByte('\n')
			}
		}
		if s := gitOut(workdir, "log", "-8", "--oneline"); s != "" {
			b.WriteString("Recent commits:\n")
			b.WriteString(s)
			if !strings.HasSuffix(s, "\n") {
				b.WriteByte('\n')
			}
		}
		if s := gitOut(workdir, "diff", "--stat", "HEAD"); s != "" {
			b.WriteString(s)
			if !strings.HasSuffix(s, "\n") {
				b.WriteByte('\n')
			}
		}
		if b.Len() > autoRecentMaxBytes {
			return b.String()[:autoRecentMaxBytes] + "\n…(git summary truncated)\n"
		}
		if b.Len() > len("Recent git changes:\n") {
			return b.String()
		}
	}
	return projectRecentFiles(workdir)
}

func gitOut(workdir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "safe.directory=*", "-C", workdir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return ""
	}
	s := string(out)
	if len(s) > autoRecentMaxBytes {
		return s[:autoRecentMaxBytes]
	}
	return s
}

type mtimeFile struct {
	rel string
	mod time.Time
}

func projectRecentFiles(workdir string) string {
	sb, err := tools.NewSandbox(workdir)
	if err != nil {
		return ""
	}
	var files []mtimeFile
	_ = filepath.WalkDir(sb.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == sb.Root {
			return nil
		}
		if tools.IsSkippedName(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		rel, rerr := filepath.Rel(sb.Root, path)
		if rerr != nil {
			return nil
		}
		files = append(files, mtimeFile{rel: filepath.ToSlash(rel), mod: info.ModTime()})
		return nil
	})
	if len(files) == 0 {
		return ""
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	if len(files) > 12 {
		files = files[:12]
	}
	var b strings.Builder
	b.WriteString("Recently modified files:\n")
	for _, f := range files {
		fmt.Fprintf(&b, "  %s\n", f.rel)
	}
	return b.String()
}

func projectKeywordHits(workdir, userText string) string {
	keys := contextKeywords(userText)
	if len(keys) == 0 {
		return ""
	}
	sb, err := tools.NewSandbox(workdir)
	if err != nil {
		return ""
	}
	var hits []string
	_ = filepath.WalkDir(sb.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || len(hits) >= autoHitMax {
			if len(hits) >= autoHitMax {
				return fs.SkipAll
			}
			return nil
		}
		if path == sb.Root {
			return nil
		}
		if tools.IsSkippedName(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, rerr := filepath.Rel(sb.Root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		low := strings.ToLower(rel)
		nameHit := false
		for _, k := range keys {
			if strings.Contains(low, k) {
				hits = append(hits, "  "+rel+" (path matches "+k+")")
				nameHit = true
				break
			}
		}
		if nameHit || len(hits) >= autoHitMax {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() > autoGrepFileMax {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil || !utf8.Valid(raw) || strings.IndexByte(string(raw), 0) >= 0 {
			return nil
		}
		text := string(raw)
		lowText := strings.ToLower(text)
		for _, k := range keys {
			idx := strings.Index(lowText, k)
			if idx < 0 {
				continue
			}
			snippet := snippetAround(text, idx, 160)
			hits = append(hits, fmt.Sprintf("  %s:%s", rel, snippet))
			break
		}
		return nil
	})
	if len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Related file hits:\n")
	for _, h := range hits {
		line := h + "\n"
		if b.Len()+len(line) > autoHitBytes {
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

func snippetAround(text string, idx, width int) string {
	start := idx - width/4
	if start < 0 {
		start = 0
	}
	end := idx + width
	if end > len(text) {
		end = len(text)
	}
	s := strings.Join(strings.Fields(text[start:end]), " ")
	if start > 0 {
		s = "…" + s
	}
	if end < len(text) {
		s += "…"
	}
	return s
}

func contextKeywords(text string) []string {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	var cur strings.Builder
	flush := func() {
		w := cur.String()
		cur.Reset()
		if len(w) < 3 || autoStop[w] || seen[w] {
			return
		}
		seen[w] = true
		out = append(out, w)
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '.' || r == '/' || r == '-' {
			cur.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

var autoStop = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true, "this": true,
	"that": true, "have": true, "will": true, "into": true, "write": true, "file": true,
	"files": true, "please": true, "just": true, "make": true, "also": true, "using": true,
	"your": true, "you": true, "are": true, "add": true, "out": true,
}
