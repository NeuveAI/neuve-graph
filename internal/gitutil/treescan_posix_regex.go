//go:build cgo && !windows

package gitutil

/*
#include <stdlib.h>
#include <locale.h>
#ifdef __GLIBC__
#include <gnu/libc-version.h>
#endif
#include <regex.h>
#ifdef __APPLE__
#include <xlocale.h>
#endif

// The character-type locale git grep would run under: setlocale(LC_CTYPE, "")
// reads LC_ALL, LC_CTYPE and LANG from the environment, and so does this.
static locale_t graph_scan_locale(void) {
	locale_t loc = newlocale(LC_CTYPE_MASK, "", (locale_t)0);
	if (loc == (locale_t)0)
		loc = newlocale(LC_CTYPE_MASK, "C", (locale_t)0);
	return loc;
}

// The C library build, whose regex engine decides lines.
static const char *graph_scan_libc(void) {
#ifdef __GLIBC__
	return gnu_get_libc_version();
#else
	return "";
#endif
}

static locale_t graph_scan_uselocale(locale_t loc) { return uselocale(loc); }

// The flags git grep -E compiles with (grep.c, compile_regexp).
static regex_t *graph_scan_regcomp(const char *pattern, int *err) {
	regex_t *re = malloc(sizeof(regex_t));
	if (re == NULL) {
		*err = REG_ESPACE;
		return NULL;
	}
	*err = regcomp(re, pattern, REG_NEWLINE | REG_EXTENDED);
	if (*err != 0) {
		free(re);
		return NULL;
	}
	return re;
}

static void graph_scan_regfree(regex_t *re) {
	regfree(re);
	free(re);
}

// git's regexec_buf from bol with no flags (look_ahead's call over the rest
// of a file): 1 with *start set to the match's offset, 0 none, -1 an error.
static int graph_scan_regexec_start(regex_t *re, const char *buf, size_t size, size_t *start) {
	regmatch_t m[1];
	m[0].rm_so = 0;
	m[0].rm_eo = (regoff_t)size;
	int rc = regexec(re, buf, 1, m, REG_STARTEND);
	if (rc == 0) {
		*start = (size_t)m[0].rm_so;
		return 1;
	}
	if (rc == REG_NOMATCH)
		return 0;
	return -1;
}

// git's regexec_buf: the whole line through REG_STARTEND, one match slot.
// 1 is a match, 0 none, -1 an engine error, which git counts as no match.
static int graph_scan_regexec(regex_t *re, const char *buf, size_t size) {
	regmatch_t m[1];
	m[0].rm_so = 0;
	m[0].rm_eo = (regoff_t)size;
	int rc = regexec(re, buf, 1, m, REG_STARTEND);
	if (rc == 0)
		return 1;
	if (rc == REG_NOMATCH)
		return 0;
	return -1;
}
*/
import "C"

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"unsafe"
)

// treeScanEngineIdentity names everything a remembered answer depends on
// besides the blob and the pattern: the scanner's rules, the platform and its
// C library (whose regex engine decides lines; on macOS it ships with the OS
// build) and the locale a git child would use.
func treeScanEngineIdentity() string {
	return strings.Join([]string{
		treeScanEngineVersion, runtime.GOOS, runtime.GOARCH, osBuild(), C.GoString(C.graph_scan_libc()),
		"LC_ALL=" + os.Getenv("LC_ALL"), "LC_CTYPE=" + os.Getenv("LC_CTYPE"), "LANG=" + os.Getenv("LANG"),
		"LOCPATH=" + os.Getenv("LOCPATH"),
	}, "\x00")
}

// Locale objects are immutable and may be installed on any number of threads,
// so one is kept per distinct locale environment, never freed.
var (
	scanLocalesMu sync.Mutex
	scanLocales   = map[string]C.locale_t{}
)

// scanLocale is the character-type locale a git child started now would use.
func scanLocale() C.locale_t {
	key := os.Getenv("LC_ALL") + "\x00" + os.Getenv("LC_CTYPE") + "\x00" + os.Getenv("LANG")
	scanLocalesMu.Lock()
	defer scanLocalesMu.Unlock()
	loc, ok := scanLocales[key]
	if !ok {
		loc = C.graph_scan_locale()
		scanLocales[key] = loc
	}
	return loc
}

// scanThread pins the calling goroutine to its OS thread and installs the scan
// locale there, so the platform engine compiles and matches under the same
// character types as a git grep child. release restores the thread.
func scanThread() (release func()) {
	loc := scanLocale()
	runtime.LockOSThread()
	previous := C.graph_scan_uselocale(loc)
	return func() {
		C.graph_scan_uselocale(previous)
		runtime.UnlockOSThread()
	}
}

// posixRegex is one compiled expression, used only on the thread (and so
// under the locale) it was compiled on.
type posixRegex struct {
	re *C.regex_t
}

func compilePOSIXRegex(pattern string) (*posixRegex, error) {
	cPattern := C.CString(pattern)
	defer C.free(unsafe.Pointer(cPattern))
	var code C.int
	re := C.graph_scan_regcomp(cPattern, &code)
	if re == nil {
		return nil, fmt.Errorf("git grep pattern %q does not compile (regcomp error %d)", pattern, int(code))
	}
	return &posixRegex{re: re}, nil
}

var emptyLine = []byte{0}

func (r *posixRegex) match(line []byte) bool {
	buf := line
	if len(buf) == 0 {
		buf = emptyLine
	}
	return C.graph_scan_regexec(r.re, (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(line))) == 1
}

// matchFrom runs the expression over buf the way git's look_ahead does and
// returns 1 with the match's start offset, 0 for no match, -1 for an engine
// error.
func (r *posixRegex) matchFrom(buf []byte) (int, int) {
	data := buf
	if len(data) == 0 {
		data = emptyLine
	}
	var start C.size_t
	rc := C.graph_scan_regexec_start(r.re, (*C.char)(unsafe.Pointer(&data[0])), C.size_t(len(buf)), &start)
	return int(rc), int(start)
}

func (r *posixRegex) free() {
	if r.re != nil {
		C.graph_scan_regfree(r.re)
		r.re = nil
	}
}
