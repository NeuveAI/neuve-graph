//go:build !windows

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-graph/internal/sem"
	"github.com/entireio/entire-graph/internal/service"
)

// withoutTimings drops every *_latency_ms value and the `service` block from
// a JSON document, the only fields two runs of one prepared read may differ
// in.
func withoutTimings(t *testing.T, data []byte) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	var strip func(any)
	strip = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for key, inner := range x {
				if strings.HasSuffix(key, "_latency_ms") || strings.HasSuffix(key, "_millis") || key == "service" {
					delete(x, key)
					continue
				}
				strip(inner)
			}
		case []any:
			for _, inner := range x {
				strip(inner)
			}
		}
	}
	strip(value)
	return value
}

// serviceBlock is the `service` block of a search's stats or of a neighbors
// read, nil when it has none.
func serviceBlock(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var value struct {
		Service map[string]any `json:"service"`
		Stats   struct {
			Service map[string]any `json:"service"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	if value.Service != nil {
		return value.Service
	}
	return value.Stats.Service
}

// A caller that joined an identical read already running gets the same bytes
// with no CPU in its `service` block, so one run's CPU is counted once; text
// that only looks like the block, inside a string, is left alone.
func TestJoinedServiceResponseCountsTheRunOnce(t *testing.T) {
	var buf bytes.Buffer
	response := sem.SearchResponse{Query: `"service":{"answered":true,"cpu_user_ms":9}`}
	response.Stats.Service = &sem.ServiceStats{Answered: true, RSSBytes: 7, MaxRSSBytes: 8, SnapshotsHeld: 1, SnapshotsLimit: 2, SnapshotHit: true, CPUUserMS: 40, CPUSystemMS: 5}
	if err := writeSearchResponse(&buf, response, "json", 0); err != nil {
		t.Fatal(err)
	}
	original := bytes.Clone(buf.Bytes())
	joined := joinedServiceResponse(service.Response{Stdout: buf.Bytes()})
	want := response
	stats := *response.Stats.Service
	stats.CPUUserMS, stats.CPUSystemMS = 0, 0
	want.Stats.Service = &stats
	var wantBuf bytes.Buffer
	if err := writeSearchResponse(&wantBuf, want, "json", 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(joined.Stdout, wantBuf.Bytes()) {
		t.Fatalf("joined answer:\n got %s\nwant %s", joined.Stdout, wantBuf.Bytes())
	}
	if !bytes.Equal(buf.Bytes(), original) {
		t.Fatal("the shared answer was changed in place")
	}
	text := service.Response{Stdout: []byte("no block here")}
	if got := joinedServiceResponse(text); string(got.Stdout) != "no block here" {
		t.Fatalf("a text answer changed: %q", got.Stdout)
	}
}

// The shared Graph service answers a prepared query and a neighbors read
// exactly as the command's own process does, starts on the first call that
// wants it, serves the next from the same process, and exits when idle.
func TestServiceAnswersAsTheProcessDoes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	work := t.TempDir()
	binary := filepath.Join(work, "entire-graph")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/entire-graph")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "auth/session.go", "package auth\n\n// NewAuthSession opens a session for a token.\nfunc NewAuthSession(token string) *Session { return &Session{Token: token} }\n\ntype Session struct{ Token string }\n\nfunc (s *Session) Close() error { return closeSession(s) }\n\nfunc closeSession(s *Session) error { return nil }\n")
	write(t, repo, "web/handler.go", "package web\n\nimport \"example.com/auth\"\n\nfunc Login(token string) error { return auth.NewAuthSession(token).Close() }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")
	cache := t.TempDir()
	tmp, err := os.MkdirTemp("", "gt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	base := append(os.Environ(), "TMPDIR="+tmp, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	var plainEnv []string
	for _, entry := range base {
		if !strings.HasPrefix(entry, envService+"=") {
			plainEnv = append(plainEnv, entry)
		}
	}
	// "require" makes a call the service did not answer fail, so a pass
	// shows the service answered.
	serviceEnv := append(append([]string(nil), plainEnv...), envService+"=require", envServiceIdle+"=10")
	runResult := func(env []string, args ...string) (stdout, stderr []byte, code int) {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Env = env
		cmd.Dir = work
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
		err := cmd.Run()
		code = 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return outBuf.Bytes(), errBuf.Bytes(), code
	}
	run := func(env []string, args ...string) []byte {
		t.Helper()
		out, stderr, code := runResult(env, args...)
		if code != 0 {
			t.Fatalf("%v: exit %d\n%s", args, code, stderr)
		}
		return out
	}
	run(plainEnv, "index", "--repo", repo, "--head", "--profile", "full", "--cache-dir", cache, "--format", "json")
	reads := [][]string{
		{"query", "--repo", repo, "--head", "--prepared-only", "--profile", "full", "--format", "json", "--top-k", "10", "--cache-dir", cache, "--query", "authentication session close"},
		{"neighbors", "--repo", repo, "--head", "--prepared-only", "--profile", "full", "--format", "json", "--symbol", "closeSession", "--cache-dir", cache},
	}
	sockets := func() []string {
		matches, _ := filepath.Glob(filepath.Join(tmp, "entire-graph-*", "*.sock"))
		return matches
	}
	// Start the service and wait for it to listen: a loaded machine can take
	// longer than a caller waits, and that caller then rightly runs the read
	// itself. Its log then starts empty.
	run(serviceEnv, reads[0]...)
	for started := time.Now(); len(sockets()) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Since(started) > time.Minute {
			t.Fatal("the service never listened")
		}
	}
	primed, _ := filepath.Glob(filepath.Join(tmp, "entire-graph-*", "*.sock.log"))
	for _, path := range primed {
		os.Remove(path)
	}
	for _, args := range reads {
		before := len(sockets())
		run(plainEnv, args...) // warm the cache directory's remembered answers
		plain := run(plainEnv, args...)
		want := withoutTimings(t, plain)
		if len(sockets()) != before {
			t.Fatal("a plain call started a service")
		}
		if block := serviceBlock(t, plain); block != nil {
			t.Fatalf("%s with the setting off reported a service block: %v", args[0], block)
		}
		firstOut := run(serviceEnv, args...)
		first := withoutTimings(t, firstOut)
		if len(sockets()) != 1 {
			t.Fatalf("the first service call left %d sockets, want one live service", len(sockets()))
		}
		secondOut := run(serviceEnv, args...)
		second := withoutTimings(t, secondOut)
		// The service says it answered, with its memory and the read's CPU;
		// the second read finds its snapshot decoded in memory.
		for index, out := range [][]byte{firstOut, secondOut} {
			block := serviceBlock(t, out)
			if block == nil || block["answered"] != true || block["reason"] != nil ||
				block["rss_bytes"].(float64) <= 0 || block["max_rss_bytes"].(float64) < block["rss_bytes"].(float64) ||
				block["snapshots_limit"] != 2.0 || block["snapshots_held"].(float64) < 1 ||
				block["cpu_user_ms"].(float64) < 0 || block["cpu_system_ms"].(float64) < 0 || block["queue_wait_ms"] != 0.0 {
				t.Fatalf("%s call %d through the service: service block %v", args[0], index+1, block)
			}
			if index == 1 && block["snapshot_hit"] != true {
				t.Fatalf("%s: the second read did not find its snapshot in memory: %v", args[0], block)
			}
		}
		if !reflect.DeepEqual(first, want) || !reflect.DeepEqual(second, want) {
			got, _ := json.Marshal(second)
			expected, _ := json.Marshal(want)
			t.Fatalf("%s through the service differs from its own process:\n got %.600s\nwant %.600s", args[0], got, expected)
		}
	}
	// Text that is not valid UTF-8 reaches the command unchanged: the same
	// output, error and exit as in its own process.
	odd := []string{"neighbors", "--repo", repo, "--head", "--prepared-only", "--profile", "full", "--format", "text", "--symbol", "zzqq\xffnothere", "--cache-dir", cache}
	wantOut, wantErr, wantCode := runResult(plainEnv, odd...)
	gotOut, gotErr, gotCode := runResult(serviceEnv, odd...)
	timing := regexp.MustCompile(`[0-9]+ms`)
	gotOut, wantOut = timing.ReplaceAll(gotOut, []byte("ms")), timing.ReplaceAll(wantOut, []byte("ms"))
	if !bytes.Equal(gotOut, wantOut) || !bytes.Equal(gotErr, wantErr) || gotCode != wantCode {
		t.Fatalf("non-UTF-8 symbol through the service:\n got %q %q %d\nwant %q %q %d", gotOut, gotErr, gotCode, wantOut, wantErr, wantCode)
	}
	// Each read the service answered left one line in its log, with the
	// block it reported.
	logs, _ := filepath.Glob(filepath.Join(tmp, "entire-graph-*", "*.sock.log"))
	if len(logs) != 1 {
		t.Fatalf("service logs %v, want one", logs)
	}
	data, err := os.ReadFile(logs[0])
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2*len(reads)+1 {
		t.Fatalf("the service log has %d lines, want %d:\n%s", len(lines), 2*len(reads)+1, data)
	}
	for _, line := range lines {
		var entry struct {
			Command string         `json:"command"`
			Service map[string]any `json:"service"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil || entry.Command == "" || entry.Service["answered"] != true {
			t.Fatalf("service log line %q: %v", line, err)
		}
	}
	deadline := time.Now().Add(40 * time.Second)
	for len(sockets()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the idle service did not exit")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A command the service could answer differently is never forwarded: a
// relative repository path, an unprepared read, or the setting left off.
func TestServiceEligibility(t *testing.T) {
	t.Setenv(envService, "auto")
	prepared := []string{"query", "--repo", "/abs/repo", "--head", "--prepared-only", "--cache-dir", "/abs/cache", "--query", "x"}
	cases := map[string]struct {
		args []string
		want bool
	}{
		"prepared and absolute": {prepared, true},
		"relative repo":         {[]string{"query", "--repo", "repo", "--head", "--prepared-only", "--cache-dir", "/abs/cache", "--query", "x"}, false},
		"relative cache":        {[]string{"query", "--repo", "/abs", "--head", "--prepared-only", "--cache-dir", "cache", "--query", "x"}, false},
		"relative ignore file":  {append(append([]string(nil), prepared...), "--ignore-file", "x.ignore"), false},
		"not prepared":          {[]string{"query", "--repo", "/abs", "--cache-dir", "/abs/cache"}, false},
		"other command":         {[]string{"index", "--repo", "/abs", "--prepared-only", "--cache-dir", "/abs/cache"}, false},
		"missing repo value":    {[]string{"query", "--prepared-only", "--cache-dir", "/abs/cache", "--repo"}, false},
		"flag as a value":       {[]string{"query", "--repo", "/abs", "--head", "--cache-dir", "/abs/cache", "--query", "--prepared-only"}, false},
		"neighbors prepared":    {[]string{"neighbors", "--repo", "/abs", "--head", "--prepared-only", "--cache-dir", "/abs/cache", "--symbol", "S"}, true},
	}
	for name, c := range cases {
		if got := serviceEligible(c.args, EntireEnv{}); got != c.want {
			t.Errorf("%s: eligible = %v, want %v", name, got, c.want)
		}
	}
	if serviceEligible(prepared, EntireEnv{SearchSession: "relative/state"}) {
		t.Error("a relative search-session file was forwarded")
	}
	t.Setenv(envService, "")
	if serviceEligible(prepared, EntireEnv{}) {
		t.Error("forwarded with the service setting off")
	}
}

// Where no service can run (here, a socket path too long to bind), an
// eligible call runs in its own process without waiting, and with
// "require" it fails instead.
func TestServiceFallbackIsImmediate(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	work := t.TempDir()
	binary := filepath.Join(work, "entire-graph")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/entire-graph").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "auth.go", "package auth\n\nfunc ValidateToken(token string) bool { return token != \"\" }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")
	cache := t.TempDir()
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 90))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "TMPDIR="+long, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	index := exec.Command(binary, "index", "--repo", repo, "--head", "--profile", "full", "--cache-dir", cache, "--format", "json")
	index.Env = env
	if out, err := index.CombinedOutput(); err != nil {
		t.Fatalf("index: %v\n%s", err, out)
	}
	query := []string{"query", "--repo", repo, "--head", "--prepared-only", "--profile", "full", "--format", "json", "--cache-dir", cache, "--query", "validate token"}
	for _, mode := range []string{"auto", "require"} {
		cmd := exec.Command(binary, query...)
		cmd.Env = append(append([]string(nil), env...), envService+"="+mode)
		started := time.Now()
		out, err := cmd.Output()
		elapsed := time.Since(started)
		if mode == "auto" && (err != nil || !bytes.Contains(out, []byte("ValidateToken")) || elapsed > 5*time.Second) {
			t.Fatalf("auto: %v after %s: %.200s", err, elapsed, out)
		}
		if mode == "auto" {
			if block := serviceBlock(t, out); block == nil || block["answered"] != false || block["reason"] != "unavailable" || block["max_rss_bytes"].(float64) <= 0 {
				t.Fatalf("auto without a service: service block %v", block)
			}
		}
		if mode == "require" && err == nil {
			t.Fatal("require answered although no service could run")
		}
	}
	// A read the service does not take (here, not a prepared read) runs in its
	// own process and says so.
	unprepared := exec.Command(binary, "query", "--repo", repo, "--head", "--profile", "full", "--format", "json", "--cache-dir", cache, "--query", "validate token")
	unprepared.Env = append(append([]string(nil), env...), envService+"=auto")
	out, err := unprepared.Output()
	if err != nil {
		t.Fatal(err)
	}
	if block := serviceBlock(t, out); block == nil || block["answered"] != false || block["reason"] != "off" {
		t.Fatalf("an unprepared read with the setting on: service block %v", block)
	}
}
