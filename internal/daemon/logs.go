package daemon

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	defaultMaxLogSize  = 50 * 1024 * 1024 // 50MB
	defaultMaxLogFiles = 5
)

// LogManager writes size-rotated service log files and fans out live lines to `yak logs -f` subscribers.
type LogManager struct {
	baseDir     string
	maxSize     int64
	maxFiles    int
	mu          sync.Mutex
	writers     map[string]*rotatingWriter
	subscribers map[string]map[chan string]struct{}
}

func NewLogManager(baseDir string) *LogManager {
	return &LogManager{
		baseDir:     baseDir,
		maxSize:     defaultMaxLogSize,
		maxFiles:    defaultMaxLogFiles,
		writers:     make(map[string]*rotatingWriter),
		subscribers: make(map[string]map[chan string]struct{}),
	}
}

func keyFor(project, service string) string {
	return project + "\x00" + service
}

func (m *LogManager) LogFilePath(project, service string) string {
	return filepath.Join(m.baseDir, project, service+".log")
}

func (m *LogManager) AppendLine(project, service, line string) error {
	key := keyFor(project, service)
	formatted := fmt.Sprintf("%s %s\n", time.Now().UTC().Format(time.RFC3339), line)

	m.mu.Lock()
	rw, ok := m.writers[key]
	if !ok {
		rw = &rotatingWriter{
			path:     m.LogFilePath(project, service),
			maxSize:  m.maxSize,
			maxFiles: m.maxFiles,
		}
		m.writers[key] = rw
	}
	subs := m.subscribers[key]
	m.mu.Unlock()

	if err := rw.WriteString(formatted); err != nil {
		return err
	}

	m.mu.Lock()
	for ch := range subs {
		select {
		case ch <- formatted:
		default:
			// Drop slow subscriber lines rather than blocking service execution.
		}
	}
	m.mu.Unlock()
	return nil
}

// Tail returns up to the last n lines from the service log file.
func (m *LogManager) Tail(project, service string, n int) ([]string, error) {
	if n <= 0 {
		n = 100
	}
	path := m.LogFilePath(project, service)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var ring []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		ring = append(ring, scanner.Text())
		if len(ring) > n {
			ring = ring[1:]
		}
	}
	return ring, scanner.Err()
}

func (m *LogManager) Subscribe(project, service string) (<-chan string, func()) {
	key := keyFor(project, service)
	ch := make(chan string, 128)

	m.mu.Lock()
	if m.subscribers[key] == nil {
		m.subscribers[key] = make(map[chan string]struct{})
	}
	m.subscribers[key][ch] = struct{}{}
	m.mu.Unlock()

	cancel := func() {
		m.mu.Lock()
		if subs := m.subscribers[key]; subs != nil {
			delete(subs, ch)
		}
		m.mu.Unlock()
		close(ch)
	}
	return ch, cancel
}

type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxSize  int64
	maxFiles int
	size     int64
}

func (w *rotatingWriter) WriteString(s string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
		return err
	}
	if info, err := os.Stat(w.path); err == nil {
		w.size = info.Size()
	} else {
		w.size = 0
	}

	if w.size+int64(len(s)) > w.maxSize {
		w.rotate()
	}

	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.WriteString(f, s)
	w.size += int64(n)
	return err
}

func (w *rotatingWriter) rotate() {
	for i := w.maxFiles - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.path, i)
		dst := fmt.Sprintf("%s.%d", w.path, i+1)
		_ = os.Rename(src, dst)
	}
	_ = os.Rename(w.path, w.path+".1")
	w.size = 0
}
