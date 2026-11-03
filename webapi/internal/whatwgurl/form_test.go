package whatwgurl_test

import (
	"slices"
	"testing"

	"github.com/MoeForge/moejs/webapi/internal/whatwgurl"
)

// formCases are WPT's url/urlencoded-parser.any.js cases.
var formCases = []struct {
	in  string
	out []whatwgurl.Pair
}{
	{"test", []whatwgurl.Pair{{"test", ""}}},
	{"\ufefftest=\ufeff", []whatwgurl.Pair{{"\ufefftest", "\ufeff"}}},
	{"%EF%BB%BFtest=%EF%BB%BF", []whatwgurl.Pair{{"\ufefftest", "\ufeff"}}},
	{"%EF%BF%BF=%EF%BF%BF", []whatwgurl.Pair{{"\uffff", "\uffff"}}},
	{"%FE%FF", []whatwgurl.Pair{{"\ufffd\ufffd", ""}}},
	{"%FF%FE", []whatwgurl.Pair{{"\ufffd\ufffd", ""}}},
	{"†&†=x", []whatwgurl.Pair{{"†", ""}, {"†", "x"}}},
	{"%C2", []whatwgurl.Pair{{"\ufffd", ""}}},
	{"%C2x", []whatwgurl.Pair{{"\ufffdx", ""}}},
	{"_charset_=windows-1252&test=%C2x", []whatwgurl.Pair{{"_charset_", "windows-1252"}, {"test", "\ufffdx"}}},
	{"", nil},
	{"a", []whatwgurl.Pair{{"a", ""}}},
	{"a=b", []whatwgurl.Pair{{"a", "b"}}},
	{"a=", []whatwgurl.Pair{{"a", ""}}},
	{"=b", []whatwgurl.Pair{{"", "b"}}},
	{"&", nil},
	{"&a", []whatwgurl.Pair{{"a", ""}}},
	{"a&", []whatwgurl.Pair{{"a", ""}}},
	{"a&a", []whatwgurl.Pair{{"a", ""}, {"a", ""}}},
	{"a&b&c", []whatwgurl.Pair{{"a", ""}, {"b", ""}, {"c", ""}}},
	{"a=b&c=d", []whatwgurl.Pair{{"a", "b"}, {"c", "d"}}},
	{"a=b&c=d&", []whatwgurl.Pair{{"a", "b"}, {"c", "d"}}},
	{"&&&a=b&&&&c=d&", []whatwgurl.Pair{{"a", "b"}, {"c", "d"}}},
	{"a=a&a=b&a=c", []whatwgurl.Pair{{"a", "a"}, {"a", "b"}, {"a", "c"}}},
	{"a==a", []whatwgurl.Pair{{"a", "=a"}}},
	{"a=a+b+c+d", []whatwgurl.Pair{{"a", "a b c d"}}},
	{"%=a", []whatwgurl.Pair{{"%", "a"}}},
	{"%a=a", []whatwgurl.Pair{{"%a", "a"}}},
	{"%a_=a", []whatwgurl.Pair{{"%a_", "a"}}},
	{"%61=a", []whatwgurl.Pair{{"a", "a"}}},
	{"%61+%4d%4D=", []whatwgurl.Pair{{"a MM", ""}}},
	{"id=0&value=%", []whatwgurl.Pair{{"id", "0"}, {"value", "%"}}},
	{"b=%2sf%2a", []whatwgurl.Pair{{"b", "%2sf*"}}},
	{"b=%2%2af%2a", []whatwgurl.Pair{{"b", "%2*f*"}}},
	{"b=%%2a", []whatwgurl.Pair{{"b", "%*"}}},
	// Bytes that are no UTF-8 after percent-decoding: U+FFFD for every
	// maximal ill-formed subsequence, as the Encoding Standard decodes.
	{"a=%F0%9F%98", []whatwgurl.Pair{{"a", "\ufffd"}}},
	{"a=%F0%9F%98x", []whatwgurl.Pair{{"a", "\ufffdx"}}},
	{"a=%E2%82", []whatwgurl.Pair{{"a", "\ufffd"}}},
	{"a=%ED%A0%80", []whatwgurl.Pair{{"a", "\ufffd\ufffd\ufffd"}}},
	{"a=%C0%80", []whatwgurl.Pair{{"a", "\ufffd\ufffd"}}},
	{"a=%F4%90%80%80", []whatwgurl.Pair{{"a", "\ufffd\ufffd\ufffd\ufffd"}}},
	{"a=%E0%80%80", []whatwgurl.Pair{{"a", "\ufffd\ufffd\ufffd"}}},
	{"a=%E2%82%AC%E2", []whatwgurl.Pair{{"a", "€\ufffd"}}},
	{"a=%80%E2%82%AC", []whatwgurl.Pair{{"a", "\ufffd€"}}},
	{"%F0%9F%98%80=%f0%9f%98%80", []whatwgurl.Pair{{"😀", "😀"}}},
	{"a=%2B+%2b", []whatwgurl.Pair{{"a", "+ +"}}},
}

func TestParseForm(t *testing.T) {
	for _, c := range formCases {
		if got := whatwgurl.ParseForm(c.in); !slices.Equal(got, c.out) {
			t.Errorf("ParseForm(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}

func TestAppendForm(t *testing.T) {
	for _, c := range []struct {
		pairs []whatwgurl.Pair
		want  string
	}{
		{nil, ""},
		{[]whatwgurl.Pair{{"", ""}}, "="},
		{[]whatwgurl.Pair{{"a b", "c+d"}}, "a+b=c%2Bd"},
		{[]whatwgurl.Pair{{"a", "1"}, {"b", "2"}}, "a=1&b=2"},
		{[]whatwgurl.Pair{{"&=%", "#?/"}}, "%26%3D%25=%23%3F%2F"},
		{[]whatwgurl.Pair{{"*-._", "!'()~"}}, "*-._=%21%27%28%29%7E"},
		{[]whatwgurl.Pair{{"é", "😀"}}, "%C3%A9=%F0%9F%98%80"},
		{[]whatwgurl.Pair{{"\x00\x1f\x7f", "AZaz09"}}, "%00%1F%7F=AZaz09"},
	} {
		if got := string(whatwgurl.AppendForm(nil, c.pairs)); got != c.want {
			t.Errorf("AppendForm(%q) = %q, want %q", c.pairs, got, c.want)
		}
	}
	if got := string(whatwgurl.AppendForm([]byte("x?"), []whatwgurl.Pair{{"a", "b"}})); got != "x?a=b" {
		t.Errorf("AppendForm onto a prefix = %q", got)
	}
}

func TestFormRoundTrip(t *testing.T) {
	for _, pairs := range [][]whatwgurl.Pair{
		{{"a", "b"}},
		{{"a b", "c+d"}, {"", ""}, {"=", "&"}},
		{{"%", "%25"}, {"%zz", "100%"}},
		{{"é", "😀\ufffd"}, {"\ufeff", "\u0000"}},
		{{"q", "SELECT * FROM t WHERE a='b' AND c=\"d\";"}},
		{{"tabs\tand\nnewlines\r", "~!@#$^&*()[]{}|\\:;'<>,.?/`"}},
	} {
		s := string(whatwgurl.AppendForm(nil, pairs))
		if got := whatwgurl.ParseForm(s); !slices.Equal(got, pairs) {
			t.Errorf("ParseForm(AppendForm(%q)) = %q (via %q)", pairs, got, s)
		}
		// Serializing what ParseForm returns gives the same string again.
		if again := string(whatwgurl.AppendForm(nil, whatwgurl.ParseForm(s))); again != s {
			t.Errorf("AppendForm(ParseForm(%q)) = %q", s, again)
		}
	}
}

func TestParseFormNoCopy(t *testing.T) {
	allocs := testing.AllocsPerRun(100, func() {
		whatwgurl.ParseForm("model=gpt&stream=true&n=1")
	})
	// The list only: names and values that need no decoding are slices.
	if allocs > 1 {
		t.Errorf("ParseForm allocates %v times, want 1", allocs)
	}
}
