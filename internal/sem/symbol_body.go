package sem

// symbolBody is one symbol's source text paired with the literal- and
// comment-stripped copy the relation scanners actually read.
//
// Sixteen scanners derived that copy independently from the same text, so
// resolving one Go function's relations stripped its body sixteen times and the
// relation phase allocated 529 MiB doing it. Deriving it once and passing it
// down removes the repeats without changing what any scanner sees.
//
// It is a value, not a cache. The relation phase resolves one symbol per
// iteration across several workers, so a package-level memo would be contended,
// and keying one by body text would be wrong the moment two symbols share a
// body.
type symbolBody struct {
	text     string
	stripped string
	// code is text through its language's own rule of what is code
	// (callCodeView), for a language whose calls say where they are, else "".
	// The scanners find calls in stripped, as always; a call's site, which a
	// line may point at, comes from code (withCodeSites).
	code string
}

func newSymbolBody(text string) symbolBody {
	return symbolBody{text: text, stripped: stripCodeLiteralsAndComments(text)}
}

// masked returns the body for text a language mask derived from this one,
// reusing the receiver when the mask changed nothing. Most languages mask
// nothing, so most symbols are stripped once even where a mask could apply.
func (b symbolBody) masked(text string) symbolBody {
	if text == b.text {
		return b
	}
	return newSymbolBody(text)
}
