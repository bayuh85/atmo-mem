package store

import "testing"

func TestTokensAndBuildMatch(t *testing.T) {
	toks := Tokens("Fix the auth bug")
	if got, want := stringsJoin(toks), "fix auth bug"; got != want {
		t.Fatalf("tokens %q", got)
	}
	if got := Tokens("the"); len(got) != 1 || got[0] != "the" {
		t.Fatalf("lone stopword = %#v", got)
	}
	if BuildMatch("???", MatchAll) != "" {
		t.Fatal("expected empty match")
	}
	if got, want := BuildMatch("Fix the auth bug", MatchAll), `"fix"* AND "auth"* AND "bug"*`; got != want {
		t.Fatalf("and match %q", got)
	}
	if got, want := BuildMatch("Fix the auth bug", MatchAny), `"fix"* OR "auth"* OR "bug"*`; got != want {
		t.Fatalf("or match %q", got)
	}
	if got, want := BuildMatch("go", MatchAny), `"go"`; got != want {
		t.Fatalf("short token %q", got)
	}
	if got, want := BuildMatch(`say "hi" now`, MatchAll), `"say"* AND "hi" AND "now"*`; got != want {
		t.Fatalf("punctuation %q", got)
	}
}

func stringsJoin(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}
