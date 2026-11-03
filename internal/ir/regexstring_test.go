package ir

import "testing"

// A regular expression written with single backslashes compiles, its
// escapes kept for the regular expression; Go's own escapes keep meaning.
func TestRegexEscapesInStrings(t *testing.T) {
	for src, want := range map[string]string{
		`matches(request.title, "\s*")`:       `\s*`,
		`matches(request.title, "^\d+\.\w$")`: `^\d+\.\w$`,
		`matches(request.title, "a\tb\"c")`:   "a\tb\"c",
		`matches(request.title, "\\s*")`:      `\s*`,
	} {
		x, err := ParseExpr(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		call, ok := x.(*Call)
		if !ok || len(call.Args) != 2 {
			t.Fatalf("%s parsed as %#v", src, x)
		}
		if lit, ok := call.Args[1].(*StrLit); !ok || lit.V != want {
			t.Errorf("%s: got %#v, want %q", src, call.Args[1], want)
		}
	}
}
