package worker

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Memory is the agent's long-term memory: Markdown files in a directory,
// indexed and searched with poma-memory (github.com/poma-ai/poma-memory). The
// agent searches it before every triage and work round and appends what a
// finished task taught it. Operators may put their own Markdown files in the
// directory too, and they are searched the same way.
//
// Everything in it comes from Slack threads or is treated as if it did, so
// search results go into the user message, never into the trusted system
// prompt the way knowledge does.
type Memory struct {
	bin     string
	dir     string // absolute
	results int
	env     []string // extra variable names for poma-memory, for tests
	key     []byte   // signs learning requests (Sign), kept in the directory

	mu  sync.Mutex // one append and index at a time
	now func() time.Time
}

// MinPomaMemory is the first poma-memory with the cosine empty gate. Without
// the gate, a search of a corpus that has no answer still returns its best bad
// match, so every task would get "related" notes.
var MinPomaMemory = [3]int{0, 4, 0}

// ImageEnv is set by the Dockerfiles to the image's name. The image without
// poma-memory refuses a config that enables memory, naming the image to run.
const ImageEnv = "ITAKEIT_AGENT_IMAGE"

// memoryImage is the image built from Dockerfile.mem.
const memoryImage = "itakeit-agent-mem"

// NewMemory returns the memory in dir, which must be absolute and writable.
func NewMemory(bin, dir string, results int) *Memory {
	return &Memory{bin: bin, dir: dir, results: results, now: time.Now}
}

func (m *Memory) db() string { return filepath.Join(m.dir, ".poma-memory.db") }

// learnDir holds the files the agent appends to, one per month.
func (m *Memory) learnDir() string { return filepath.Join(m.dir, "learnings") }

// memEnvNames is all poma-memory gets. POMA_MEMORY_EMPTY_GATE and POMA_EMBEDDER
// are left out on purpose: either can switch the empty gate off, and the check
// at startup cannot see what a variable would change later. The Slack tokens
// never reach it. The HF_* variables point it at the embedding model, which the
// memory image carries (HF_HUB_OFFLINE=1, so nothing is downloaded at run time).
var memEnvNames = []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TMPDIR", "XDG_CACHE_HOME",
	"HF_HOME", "HF_HUB_CACHE", "HF_HUB_OFFLINE", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE"}

func (m *Memory) run(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.bin, args...)
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return !slices.Contains(memEnvNames, name) && !slices.Contains(m.env, name)
	})
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := firstLine(strings.TrimSpace(stderr.String())); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return out, fmt.Errorf("poma-memory %s: %w", args[0], err)
	}
	return out, nil
}

// Check fails startup unless poma-memory can give answers worth trusting: it
// is installed, at least MinPomaMemory, runs with semantic search, and its
// empty gate suppresses an unrelated query (which needs semantic search: BM25
// has no score to gate on). It then indexes the directory.
// The probe runs on a corpus of its own, so it proves the same thing whatever
// the directory holds.
func (m *Memory) Check(ctx context.Context) error {
	if img := os.Getenv(ImageEnv); img != "" && img != memoryImage {
		return fmt.Errorf("agent.memory is enabled, but this is the %s image, which has no poma-memory: run the %s image, built from Dockerfile.mem (docker build -f Dockerfile.mem -t %s .)", img, memoryImage, memoryImage)
	}
	out, err := m.run(ctx, time.Minute, "--version")
	if err != nil {
		return fmt.Errorf("agent.memory needs poma-memory, and running %q failed (%v): run the %s image, built from Dockerfile.mem, or install 'poma-memory[semantic]' >= %d.%d.%d and set agent.memory.bin", m.bin, err, memoryImage, MinPomaMemory[0], MinPomaMemory[1], MinPomaMemory[2])
	}
	if v := strings.TrimSpace(string(out)); !versionAtLeast(v, MinPomaMemory) {
		return fmt.Errorf("agent.memory: poma-memory %q is older than %d.%d.%d, which added the empty gate: without it every task gets unrelated notes", v, MinPomaMemory[0], MinPomaMemory[1], MinPomaMemory[2])
	}
	if err := m.probe(ctx); err != nil {
		return fmt.Errorf("agent.memory: %w", err)
	}
	if err := os.MkdirAll(m.learnDir(), 0o700); err != nil {
		return fmt.Errorf("agent.memory.dir must be writable: %w", err)
	}
	if err := m.loadKey(); err != nil {
		return fmt.Errorf("agent.memory: %w", err)
	}
	if _, err := m.run(ctx, 10*time.Minute, "index", m.dir, "--db", m.db()); err != nil {
		return fmt.Errorf("agent.memory: index %s: %w", m.dir, err)
	}
	return nil
}

const (
	probeDoc = "# Notes\n\n## Ingress\n\nThe staging ingress controller restarts when its TLS secret rotates. Restart the ingress deployment after rotating certificates.\n\n## CI\n\nGo module proxy errors on CI go away with GOFLAGS=-mod=mod.\n"
	// probeHit must find the ingress note. probeMiss shares a word with the
	// corpus ("deployment"), so BM25 alone matches it: only the empty gate, which
	// needs semantic search, can return nothing for it.
	probeHit  = "ingress keeps restarting after certificate rotation"
	probeMiss = "the sales team deployment of the quarterly budget"
)

func (m *Memory) probe(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "itakeit-agent-memprobe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte(probeDoc), 0o600); err != nil {
		return err
	}
	db := filepath.Join(dir, ".poma-memory.db")
	if _, err := m.run(ctx, 5*time.Minute, "index", dir, "--db", db); err != nil {
		return err
	}
	hits, err := m.search(ctx, dir, db, probeHit)
	switch {
	case err != nil:
		return err
	case len(hits) == 0:
		return fmt.Errorf("the startup probe found nothing for %q in a note about exactly that", probeHit)
	}
	if hits, err = m.search(ctx, dir, db, probeMiss); err != nil {
		return err
	}
	// `status` is no proof of semantic search: it reported "Semantic: yes" with
	// the model missing, while search ran on BM25 alone. This query is the proof.
	if len(hits) > 0 {
		return fmt.Errorf("the empty gate is not working: the unrelated query %q returned notes. Install 'poma-memory[semantic]' (model2vec) with its model, since the gate needs semantic search", probeMiss)
	}
	return nil
}

// loadKey reads the signing key, or creates it on first start. It lives in the
// memory directory so requests posted before a restart still verify after it.
func (m *Memory) loadKey() error {
	path := filepath.Join(m.dir, ".approval-key")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key := make([]byte, 32)
		rand.Read(key) // never fails (crypto/rand)
		raw = []byte(hex.EncodeToString(key))
		err = os.WriteFile(path, raw, 0o600)
	}
	if err != nil {
		return fmt.Errorf("signing key: %w", err)
	}
	if m.key, err = hex.DecodeString(strings.TrimSpace(string(raw))); err != nil || len(m.key) < 32 {
		return fmt.Errorf("signing key %s is damaged: remove it, which voids pending learning requests", path)
	}
	return nil
}

// Sign binds a learning to its task, so a learning request can be verified
// when it is read back from Slack. Model output never carries a valid one.
func (m *Memory) Sign(taskID, text string) string {
	if m.key == nil {
		panic("worker: Memory.Sign before Check")
	}
	h := hmac.New(sha256.New, m.key)
	h.Write([]byte(taskID + "\x00" + text))
	return hex.EncodeToString(h.Sum(nil))
}

type memHit struct {
	FilePath string  `json:"file_path"`
	Score    float64 `json:"score"`
	Context  string  `json:"context"`
}

// search runs one query. The query is Slack text, so it goes after "--",
// where poma-memory cannot read it as an option. --socket off keeps a search
// daemon of some other poma-memory, with another version or settings, out of it.
func (m *Memory) search(ctx context.Context, dir, db, query string) ([]memHit, error) {
	out, err := m.run(ctx, 2*time.Minute, "search", "--path", dir, "--db", db, "--top", strconv.Itoa(m.results), "--json", "--socket", "off", "--", cut(query, 2000))
	if err != nil {
		return nil, err
	}
	var hits []memHit
	if err := json.Unmarshal(out, &hits); err != nil {
		return nil, fmt.Errorf("poma-memory search: %w", err)
	}
	return hits, nil
}

// maxRecall caps what a search adds to a request.
const maxRecall = 16 << 10

// Recall returns the notes that match query, each under a line naming its file
// relative to the memory directory, or "" when nothing matches.
func (m *Memory) Recall(ctx context.Context, query string) (string, error) {
	hits, err := m.search(ctx, m.dir, m.db(), query)
	if err != nil || len(hits) == 0 {
		return "", err
	}
	// poma-memory reports resolved paths, so the directory is compared resolved
	// too (on macOS /var is a link to /private/var).
	roots := []string{m.dir}
	if real, err := filepath.EvalSymlinks(m.dir); err == nil && real != m.dir {
		roots = append(roots, real)
	}
	var b strings.Builder
	for _, h := range hits {
		name := filepath.Base(h.FilePath) // the model never sees the agent's paths
		for _, root := range roots {
			if rel, err := filepath.Rel(root, h.FilePath); err == nil && !strings.HasPrefix(rel, "..") {
				name = rel
				break
			}
		}
		part := fmt.Sprintf("===== %s =====\n%s\n\n", filepath.ToSlash(name), strings.TrimSpace(h.Context))
		if b.Len()+len(part) > maxRecall {
			break
		}
		b.WriteString(part)
	}
	return strings.TrimSpace(b.String()), nil
}

// CleanLearning makes \r\n and \r line breaks and drops the other control
// characters but tab. poma-memory reads files in Python's text mode, which
// turns a lone \r into a line break, so a \r that quote left inside one line
// would start a heading or a fence there. Work applies it before the learning
// is signed and posted, so Slack, the signature and the file see one text.
func CleanLearning(text string) string {
	text = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text)
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f || r == 0x85 || r == 0x2028 || r == 0x2029 {
			return -1
		}
		return r
	}, text)
}

// quote makes a learning one block quote with no fences, so nothing in it can
// start a heading or a code block and cut the file's structure, which
// poma-memory chunks by: every later entry would lose its heading.
func quote(text string) string {
	text = strings.ReplaceAll(strings.TrimSpace(CleanLearning(text)), "```", "``\u200b`")
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight("> "+strings.TrimSpace(l), " ")
	}
	return strings.Join(lines, "\n")
}

// Save appends a learning under a heading naming its task, then indexes the
// file. poma-memory only reads what was appended. Once the append succeeded
// the learning is saved: a failed index is logged, since the next start
// indexes the file anyway, and an error would invite a retry that appends it
// twice.
func (m *Memory) Save(ctx context.Context, id, text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("empty learning")
	}
	if !taskID.MatchString(id) {
		return fmt.Errorf("task id %q is not a message ts", id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	file := filepath.Join(m.learnDir(), now.Format("2006-01")+".md")
	if err := os.MkdirAll(m.learnDir(), 0o700); err != nil {
		return err
	}
	// Saving twice is a no-op: an approval whose "saved" update was lost can be
	// approved again after a restart, and a reply can repeat a request.
	body := fmt.Sprintf(", task %s\n\n%s\n", id, quote(text))
	if files, err := filepath.Glob(filepath.Join(m.learnDir(), "*.md")); err == nil {
		for _, f := range files {
			if raw, err := os.ReadFile(f); err == nil && bytes.Contains(raw, []byte(body)) {
				return nil
			}
		}
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	var entry string
	if st, err := f.Stat(); err == nil && st.Size() == 0 {
		entry = "# Learnings " + now.Format("2006-01") + "\n"
	}
	entry += "\n## " + now.Format("2006-01-02") + body
	_, err = f.WriteString(entry)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if _, err := m.run(ctx, 5*time.Minute, "index", m.dir, "--file", file, "--db", m.db()); err != nil {
		slog.Warn("learning saved, but indexing it failed: it is searchable after the next start", "file", file, "err", err)
	}
	return nil
}

// versionAtLeast compares the leading numbers of v's first three parts, so
// 0.5.0rc1 reads as 0.5.0. A version it cannot read counts as too old.
func versionAtLeast(v string, min [3]int) bool {
	parts := strings.SplitN(v, ".", 4)
	if len(parts) < 3 {
		return false
	}
	for i := range 3 {
		digits := parts[i]
		if j := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); j >= 0 {
			digits = digits[:j]
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			return false
		}
		if n != min[i] {
			return n > min[i]
		}
	}
	return true
}
