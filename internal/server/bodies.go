package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/xinyao27/jevonian/internal/paths"
)

// Port of the request half of src/bodies.ts: each routed turn dumps its
// request body and routing decision to <dataDir>/bodies/<requestId>.json so
// the log detail view (GET /api/logs/:id) can show it. Writes are queued off
// the request path, files are 0600, and only the newest maxBodies are kept.

const (
	maxBodies  = 1000
	pruneEvery = 100
)

var safeBodyID = regexp.MustCompile(`^[0-9a-fA-F-]{8,64}$`)

// bodyWriter drains capture writes in order on one goroutine so a burst of
// turns never blocks a request on disk I/O and a sweep sees a stable directory.
type bodyWriter struct {
	mu         sync.Mutex
	queue      []bodyJob
	running    bool
	sincePrune int
	idle       *sync.Cond
}

type bodyJob struct {
	dir  string
	id   string
	data []byte
}

var bodies = newBodyWriter()

func newBodyWriter() *bodyWriter {
	w := &bodyWriter{}
	w.idle = sync.NewCond(&w.mu)
	return w
}

// captureEnabled is false only when JEVONIAN_CAPTURE_BODIES=0.
func captureEnabled() bool { return os.Getenv("JEVONIAN_CAPTURE_BODIES") != "0" }

// SaveBody queues a capture. It never blocks on disk and never fails the
// caller: an unwritable or unserializable capture is dropped silently.
func SaveBody(id string, payload any) {
	if !captureEnabled() || !safeBodyID.MatchString(id) {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	// Resolve the directory now, not in the worker, so a data-dir change between
	// the call and the write cannot split the mkdir from the path.
	bodies.enqueue(bodyJob{dir: filepath.Join(paths.DataDir(), "bodies"), id: id, data: data})
}

// FlushBodies waits for queued captures to reach disk (tests, shutdown).
func FlushBodies() { bodies.flush() }

func (w *bodyWriter) enqueue(job bodyJob) {
	w.mu.Lock()
	w.queue = append(w.queue, job)
	start := !w.running
	w.running = true
	w.mu.Unlock()
	if start {
		go w.drain()
	}
}

func (w *bodyWriter) flush() {
	w.mu.Lock()
	for w.running {
		w.idle.Wait()
	}
	w.mu.Unlock()
}

func (w *bodyWriter) drain() {
	for {
		w.mu.Lock()
		if len(w.queue) == 0 {
			w.running = false
			w.idle.Broadcast()
			w.mu.Unlock()
			return
		}
		job := w.queue[0]
		w.queue = w.queue[1:]
		w.mu.Unlock()

		writeBody(job)
		w.mu.Lock()
		w.sincePrune++
		sweep := len(w.queue) == 0 && w.sincePrune >= pruneEvery
		if sweep {
			w.sincePrune = 0
		}
		w.mu.Unlock()
		if sweep {
			pruneBodies(job.dir)
		}
	}
}

func writeBody(job bodyJob) {
	if err := os.MkdirAll(job.dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(job.dir, job.id+".json")
	if err := os.WriteFile(path, job.data, 0o600); err != nil {
		return
	}
	_ = os.Chmod(path, 0o600)
}

// pruneBodies trims the directory oldest-first down to maxBodies files.
func pruneBodies(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type aged struct {
		name string
		at   time.Time
	}
	var files []aged
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, aged{e.Name(), info.ModTime()})
	}
	if len(files) <= maxBodies {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.Before(files[j].at) })
	for _, f := range files[:len(files)-maxBodies] {
		_ = os.Remove(filepath.Join(dir, f.name))
	}
}
