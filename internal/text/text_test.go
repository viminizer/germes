package text

import "testing"

func TestReplace(t *testing.T) {
	s := `say "hi" and "hi"`
	if _, _, err := Replace(s, `"hi"`, `"yo"`, false); err == nil {
		t.Fatal("two matches replaced without all")
	}
	if got, n, err := Replace(s, `"hi"`, `"yo"`, true); err != nil || n != 2 || got != `say "yo" and "yo"` {
		t.Fatalf("all: %q %d %v", got, n, err)
	}
	// The model escaped quotes that the text does not escape.
	if got, n, err := Replace(`a "b" c`, `\"b\"`, `\"d\"`, false); err != nil || n != 1 || got != `a "d" c` {
		t.Fatalf("unescape: %q %d %v", got, n, err)
	}
}
