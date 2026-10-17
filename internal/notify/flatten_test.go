package notify

import (
	"regexp"
	"testing"

	"github.com/KamalF/nag/web"
)

// The §9.10 case table, every row. Whatever renders as literal text
// client-side must stay byte-identical here. Only a real link collapses
// to its label.
func TestFlattenLinks(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"one link", "[docs](https://x/y)", "docs"},
		{"text, link, text", "see [docs](https://x) now", "see docs now"},
		{"two links", "[a](https://x) and [b](https://y)", "a and b"},
		{"javascript scheme stays literal", "[a](javascript:alert(1))", "[a](javascript:alert(1))"},
		{"mailto stays literal", "[a](mailto:x@y.z)", "[a](mailto:x@y.z)"},
		{"unencoded paren stays literal", "[Foo](https://w/Foo_(bar))", "[Foo](https://w/Foo_(bar))"},
		{"percent-encoded parens link", "[Foo](https://w/Foo_%28bar%29)", "Foo"},
		{"empty label stays literal", "[](https://x)", "[](https://x)"},
		{"unclosed target stays literal", "[a](https://x", "[a](https://x"},
		{"no opening bracket stays literal", "a](b)", "a](b)"},
		{"nested brackets stay literal", "[[a]](b)", "[[a]](b)"},
		{"no autolinking", "https://x/y", "https://x/y"},
		{"http scheme links too", "[a](http://x)", "a"},
		{"plain text untouched", "buy milk", "buy milk"},
		{"empty target stays literal", "[a]()", "[a]()"},
		{"relative target stays literal", "see [note](internal)", "see [note](internal)"},
		{"scheme is case-insensitive", "[a](HTTPS://x)", "a"},
		{"a bare percent still links", "[sale](https://shop.example/50%)", "sale"},
		{"link amid literals", "[a](ftp://x) then [b](https://y)", "[a](ftp://x) then b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := flattenLinks(tt.in); got != tt.want {
				t.Errorf("flattenLinks(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// §9.10: the parser exists twice, here and in the app.js renderer, and both
// must use the same regex. The JS literal is read from the file that the
// binary embeds, so a change on only one side fails here.
func TestClientLinkRegexMatchesFlattenLinks(t *testing.T) {
	raw, err := web.Files.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^const LINK = /(.*)/g;$`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("no `const LINK = /…/g;` in web/app.js")
	}
	if got, want := string(m[1]), linkPattern.String(); got != want {
		t.Errorf("app.js LINK = %s, flattenLinks uses %s", got, want)
	}
}
