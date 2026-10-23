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
)

// withoutTimings drops every *_latency_ms value from a JSON document, the
// only fields two runs of one prepared read may differ in.
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
				if strings.HasSuffix(key, "_latency_ms") || strings.HasSuffix(key, "_millis") {
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
	serviceEnv := append(append([]string(nil), plainEnv...), envService+"=require", envServiceIdle+"=2")
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
	for _, args := range reads {
		before := len(sockets())
		run(plainEnv, args...) // warm the cache directory's remembered answers
		want := withoutTimings(t, run(plainEnv, args...))
		if len(sockets()) != before {
			t.Fatal("a plain call started a service")
		}
		first := withoutTimings(t, run(serviceEnv, args...))
		if len(sockets()) != 1 {
			t.Fatalf("the first service call left %d sockets, want one live service", len(sockets()))
		}
		second := withoutTimings(t, run(serviceEnv, args...))
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
	deadline := time.Now().Add(15 * time.Second)
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
		if mode == "require" && err == nil {
			t.Fatal("require answered although no service could run")
		}
	}
}
