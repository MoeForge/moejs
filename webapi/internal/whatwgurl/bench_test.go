package whatwgurl_test

import (
	"testing"

	"github.com/MoeForge/moejs/webapi/internal/whatwgurl"
)

func BenchmarkParse(b *testing.B) {
	base, _ := whatwgurl.Parse("https://api.openai.com/v1/chat/completions", nil)
	for _, c := range []struct {
		name, in string
		base     *whatwgurl.URL
	}{
		{"fetch", "https://api.openai.com/v1/chat/completions?x=1", nil},
		{"relative", "../models?limit=10", base},
		{"normalize", "HTTPS://API.OpenAI.com:443/v1/./chat/../models?q=a b", nil},
		{"non-ASCII host", "https://例え.テスト/v1/chat/completions", nil},
		{"IPv6", "http://[2001:db8::1]:8080/", nil},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := whatwgurl.Parse(c.in, c.base); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSetSearch(b *testing.B) {
	u, _ := whatwgurl.Parse("https://api.openai.com/v1/chat/completions", nil)
	b.ReportAllocs()
	for b.Loop() {
		c := *u
		c.SetSearch("?model=gpt-4o&stream=true")
	}
}

func BenchmarkParseForm(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		whatwgurl.ParseForm("model=gpt-4o&stream=true&q=a+b%20c")
	}
}
