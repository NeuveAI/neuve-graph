package sem

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

// The hand-written scanner returns exactly the regular expression's words,
// for any text: invalid UTF-8, letters and numbers of many scripts (Nd, Nl,
// No), marks, symbols, and every punctuation inside and outside the class.
func TestSearchSourceWordsMatchesThePattern(t *testing.T) {
	alphabet := []string{
		"a", "Z", "é", "ß", "Ж", "日", "ٔ", "́", "١", "Ⅻ", "½", "²", "_", ".", "/", ":", "+", "#", "-",
		" ", "\t", "\n", "\r", "(", ")", ",", ";", "'", "\"", "@", "$", "%", "&", "*", "=", "€", "😀",
		"\xff", "\xc3", "\xe2\x82", "\x00", "�", "0", "9", "ǅ", "ʰ", "々",
	}
	rng := rand.New(rand.NewSource(11))
	for trial := 0; trial < 20000; trial++ {
		var b strings.Builder
		for n := rng.Intn(24); n >= 0; n-- {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		text := b.String()
		want := searchSourceWordPattern.FindAllString(text, -1)
		if got := searchSourceWords(text); !reflect.DeepEqual(got, want) {
			t.Fatalf("text %q: words %q, want %q", text, got, want)
		}
	}
}

func FuzzSearchSourceWords(f *testing.F) {
	f.Add("func AuthSession(ctx context.Context) // café ½ \xff end")
	f.Fuzz(func(t *testing.T, text string) {
		if got, want := searchSourceWords(text), searchSourceWordPattern.FindAllString(text, -1); !reflect.DeepEqual(got, want) {
			t.Fatalf("text %q: words %q, want %q", text, got, want)
		}
	})
}

// A remembered split is the computed one, and appending to it never changes
// what the next caller gets.
func TestSearchTokenVariantsMemoryIsTheSplit(t *testing.T) {
	for _, word := range []string{"AuthSessionToken", "HTTPServer2", "sessions", "x", "", "./a-b:c", "Größe", "IDs"} {
		want := computeSearchTokenVariants(word)
		first := searchTokenVariants(word)
		if !reflect.DeepEqual(first, want) {
			t.Fatalf("%q: %q, want %q", word, first, want)
		}
		_ = append(first, "appended")
		if again := searchTokenVariants(word); !reflect.DeepEqual(again, want) {
			t.Fatalf("%q after an append: %q, want %q", word, again, want)
		}
	}
}

// The memory keeps copies, never slices of the text it was asked about (which
// would keep whole files alive), skips very long words, and stays within its
// byte bound however many words it sees.
func TestSearchTokenVariantsMemoryIsBounded(t *testing.T) {
	file := strings.Repeat("filler ", 1000) + "remembered_WordHere and more"
	word := file[strings.Index(file, "remembered"):strings.Index(file, " and")]
	variants := searchTokenVariants(word)
	searchTokenVariantsMemo.RLock()
	stored := searchTokenVariantsMemo.words[word]
	var key string
	for k := range searchTokenVariantsMemo.words {
		if k == word {
			key = k
		}
	}
	searchTokenVariantsMemo.RUnlock()
	inFile := func(s string) bool {
		if len(s) == 0 {
			return false
		}
		start := uintptr(unsafe.Pointer(unsafe.StringData(file)))
		at := uintptr(unsafe.Pointer(unsafe.StringData(s)))
		return at >= start && at < start+uintptr(len(file))
	}
	if inFile(key) {
		t.Fatal("the remembered word points into the text it came from")
	}
	for _, v := range append(stored, variants...) {
		if inFile(v) {
			t.Fatalf("remembered part %q points into the text it came from", v)
		}
	}
	long := strings.Repeat("a", maxRememberedSearchWord+1)
	searchTokenVariants(long)
	searchTokenVariantsMemo.RLock()
	_, kept := searchTokenVariantsMemo.words[long]
	searchTokenVariantsMemo.RUnlock()
	if kept {
		t.Fatal("a word over the length bound was remembered")
	}
	for i := 0; i < 400000; i++ {
		searchTokenVariants(fmt.Sprintf("Word%dPart%d", i, i*7))
	}
	searchTokenVariantsMemo.RLock()
	size := searchTokenVariantsMemo.bytes
	searchTokenVariantsMemo.RUnlock()
	if size > maxSearchTokenVariantsMemoBytes {
		t.Fatalf("the memory holds %d bytes, over its %d-byte bound", size, maxSearchTokenVariantsMemoBytes)
	}
}
