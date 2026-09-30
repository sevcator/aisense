package debuglog

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const redacted = "[REDACTED]"

type Logger struct {
	mu               sync.Mutex
	path             string
	file             *os.File
	size             int64
	maxSize          int64
	backups          int
	closed           bool
	dir              string
	enabled          func() bool
	now              func() time.Time
	lastResponseOnly bool
}

func New(path string, maxSize int64, backups int) (*Logger, error) {
	return newLogger(path, maxSize, backups, false)
}

// NewRaw is retained for existing callers; credentials are now always redacted.
func NewRaw(path string, maxSize int64, backups int) (*Logger, error) {
	return New(path, maxSize, backups)
}

// NewDynamic stays wired for the process lifetime. An empty override uses local
// calendar dates beside the config. The predicate must be lock-free.
func NewDynamic(dir, override string, enabled func() bool) *Logger {
	l := &Logger{path: override, enabled: enabled, now: time.Now, maxSize: -1}
	if override == "" {
		l.dir = dir
		if l.dir == "" {
			l.dir = "."
		}
	}
	return l
}

// NewLastResponseDynamic writes only completed, idle conversation responses.
func NewLastResponseDynamic(dir, override string, enabled func() bool) *Logger {
	l := NewDynamic(dir, override, enabled)
	l.lastResponseOnly = true
	return l
}

func (l *Logger) LastResponseOnly() bool { return l != nil && l.lastResponseOnly }

func (l *Logger) Enabled() bool {
	return l != nil && (l.enabled == nil || l.enabled())
}

// Validate is called before publishing/persisting enabled configuration. All
// file operations share Event's lock, including rotation and shutdown.
func (l *Logger) Validate(enabled bool) error {
	if !enabled {
		return nil
	}
	if l == nil {
		return fmt.Errorf("logging is not configured")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return fmt.Errorf("logger is closed")
	}
	if err := l.openLocked(); err != nil {
		return err
	}
	if !l.lastResponseOnly {
		if _, err := l.file.WriteString("{\"event\":\"logging.write_check\"}\n"); err != nil {
			return err
		}
	}
	return l.file.Sync()
}

func (l *Logger) openLocked() error {
	path := l.path
	if l.dir != "" {
		path = filepath.Join(l.dir, l.now().Format("2006-01-02")+".log")
	}
	if l.file != nil && path == l.path {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if l.file != nil {
		if err := l.file.Close(); err != nil {
			log.Printf("[logging] close: %v", err)
		}
	}
	l.file, l.path = f, path
	return nil
}

func newLogger(path string, maxSize int64, backups int, _ bool) (*Logger, error) {
	if maxSize == 0 {
		maxSize = 64 << 20
	}
	if backups < 1 {
		backups = 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	st, _ := f.Stat()
	l := &Logger{path: path, file: f, maxSize: maxSize, backups: backups}
	if st != nil {
		l.size = st.Size()
	}
	return l, nil
}

func (l *Logger) Path() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.dir != "" {
		return filepath.Join(l.dir, l.now().Format("2006-01-02")+".log")
	}
	return l.path
}

func (l *Logger) Event(traceID, event string, fields map[string]any) {
	if !l.Enabled() {
		return
	}
	if l.lastResponseOnly && event != "conversation.last_response" {
		return
	}
	record := map[string]any{
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"event":     event,
	}
	if traceID != "" {
		record["trace_id"] = traceID
	}
	for k, v := range fields {
		record[k] = sanitizeAny(k, v)
	}
	line, err := json.Marshal(record)
	if err != nil {
		log.Printf("[logging] encode event: %v", err)
		return
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || !l.Enabled() {
		return
	}
	if err := l.openLocked(); err != nil {
		log.Printf("[logging] open: %v", err)
		return
	}
	if l.maxSize > 0 && l.size > 0 && l.size+int64(len(line)) > l.maxSize {
		if err := l.rotateLocked(); err != nil {
			log.Printf("[logging] rotate: %v", err)
			return
		}
	}
	n, err := l.file.Write(line)
	if err == nil {
		l.size += int64(n)
	} else {
		log.Printf("[logging] write: %v", err)
	}
}

func (l *Logger) rotateLocked() error {
	if err := l.file.Close(); err != nil {
		return err
	}
	_ = os.Remove(l.path + "." + itoa(l.backups))
	for i := l.backups - 1; i >= 1; i-- {
		_ = os.Rename(l.path+"."+itoa(i), l.path+"."+itoa(i+1))
	}
	_ = os.Rename(l.path, l.path+".1")
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	l.file = f
	l.size = 0
	return nil
}

func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if l.file == nil {
		return nil
	}
	return l.file.Close()
}

// Headers returns a redacted copy, including for nil loggers.
func (l *Logger) Headers(headers http.Header) map[string][]string {
	return RedactHeaders(headers)
}

// URL redacts userinfo and sensitive query params. Nil-safe.
func (l *Logger) URL(raw string) string {
	return RedactURL(raw)
}

// Body renders complete payloads with sensitive structured fields masked.
func (l *Logger) Body(body []byte, contentType string) any {
	if l != nil && (!l.Enabled() || l.lastResponseOnly) {
		return nil
	}
	return Body(body, contentType)
}

func RedactHeaders(headers http.Header) map[string][]string {
	out := make(map[string][]string, len(headers))
	for k, values := range headers {
		if sensitiveKey(k) {
			out[k] = []string{redacted}
			continue
		}
		out[k] = append([]string(nil), values...)
	}
	return out
}

func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[INVALID URL]"
	}
	if u.User != nil {
		u.User = url.User(redacted)
	}
	q := u.Query()
	for key := range q {
		if sensitiveKey(key) {
			q.Set(key, redacted)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func Body(body []byte, contentType string) any {
	if len(body) == 0 {
		return map[string]any{"encoding": "text", "value": "", "bytes": 0}
	}
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		lines := strings.Split(string(body), "\n")
		var out, data []string
		var indexes []int
		flush := func() {
			var value any
			if len(indexes) > 0 && json.Unmarshal([]byte(strings.Join(data, "\n")), &value) == nil {
				clean, _ := json.Marshal(sanitizeAny("", value))
				out[indexes[0]] = "data: " + string(clean)
				for _, i := range indexes[1:] {
					out[i] = ""
				}
			}
			data, indexes = nil, nil
		}
		for _, line := range lines {
			line = strings.TrimSuffix(line, "\r")
			out = append(out, line)
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimPrefix(line[5:], " "))
				indexes = append(indexes, len(out)-1)
			}
			if line == "" {
				flush()
			}
		}
		flush()
		return map[string]any{"encoding": "text", "value": strings.Join(out, "\n"), "bytes": len(body)}
	}
	if strings.Contains(strings.ToLower(contentType), "json") || json.Valid(body) {
		var value any
		if json.Unmarshal(body, &value) == nil {
			return map[string]any{"encoding": "json", "value": sanitizeAny("", value), "bytes": len(body)}
		}
	}
	if strings.Contains(strings.ToLower(contentType), "application/x-www-form-urlencoded") {
		if values, err := url.ParseQuery(string(body)); err == nil {
			for key := range values {
				if sensitiveKey(key) {
					values.Set(key, redacted)
				}
			}
			return map[string]any{"encoding": "form", "value": values.Encode(), "bytes": len(body)}
		}
	}
	if utf8.Valid(body) {
		return map[string]any{"encoding": "text", "value": string(body), "bytes": len(body)}
	}
	return map[string]any{"encoding": "base64", "value": base64.StdEncoding.EncodeToString(body), "bytes": len(body)}
}

func sanitizeAny(key string, value any) any {
	if sensitiveKey(key) {
		return redacted
	}
	switch v := value.(type) {
	case string:
		if strings.Contains(strings.ToLower(key), "url") {
			return RedactURL(v)
		}
		if key == "error" {
			return errorURL.ReplaceAllStringFunc(v, RedactURL)
		}
		return v
	case map[string][]string:
		return RedactHeaders(http.Header(v))
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, child := range v {
			out[k] = sanitizeAny(k, child)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(v))
		for k, child := range v {
			if sensitiveKey(k) {
				out[k] = redacted
			} else {
				out[k] = child
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = sanitizeAny("", child)
		}
		return out
	default:
		// Structs (notably config snapshots) must also pass through redaction.
		b, err := json.Marshal(value)
		if err == nil && len(b) > 0 && (b[0] == '{' || b[0] == '[') {
			var plain any
			if json.Unmarshal(b, &plain) == nil {
				return sanitizeAny(key, plain)
			}
		}
		return value
	}
}

var errorURL = regexp.MustCompile(`https?://[^\s"<>]+`)

// Stream buffers complete SSE frames, never transport fragments or truncated
// scanner tokens, so a credential split across reads is still redacted.
// Each stream is owned by one request goroutine.
type Stream struct {
	Logger  *Logger
	Emit    func(any)
	pending []byte
}

func (s *Stream) Write(b []byte) {
	if !s.Logger.Enabled() {
		s.pending = nil
		return
	}
	s.pending = append(s.pending, b...)
	for {
		i, delimiter := bytes.Index(s.pending, []byte("\n\n")), 2
		if crlf := bytes.Index(s.pending, []byte("\r\n\r\n")); crlf >= 0 && (i < 0 || crlf < i) {
			i, delimiter = crlf, 4
		}
		if i < 0 {
			break
		}
		s.Emit(s.Logger.Body(s.pending[:i+delimiter], "text/event-stream"))
		s.pending = s.pending[i+delimiter:]
	}
	if len(s.pending) == 0 {
		s.pending = nil
	}
}

func (s *Stream) Close() {
	if len(s.pending) > 0 && s.Logger.Enabled() {
		s.Emit(s.Logger.Body(s.pending, "text/event-stream"))
	}
	s.pending = nil
}

func sensitiveKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.NewReplacer("-", "", "_", "", ".", "").Replace(key)
	switch key {
	case "authorization", "proxyauthorization", "cookie", "setcookie", "xapikey",
		"apikey", "apikeys", "key", "credentials", "session", "sessionid", "totp", "password", "clientsecret", "secret", "accesstoken", "refreshtoken",
		"idtoken", "token", "code", "codeverifier", "assertion":
		return true
	}
	return strings.HasSuffix(key, "password") || strings.HasSuffix(key, "secret") || strings.HasSuffix(key, "token") || strings.HasSuffix(key, "apikey") || strings.HasSuffix(key, "apikeys")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
