package markdown

import "testing"

func TestRender(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"paragraph", "hello world", "<p>hello world</p>"},
		{"paragraph joins wrapped lines", "hello\nworld", "<p>hello world</p>"},
		{"two paragraphs", "one\n\ntwo", "<p>one</p><p>two</p>"},
		{"heading is shifted down one level", "# Title", "<h2>Title</h2>"},
		{"deepest heading is not shifted past h6", "###### Deep", "<h6>Deep</h6>"},
		{"bold", "a **b** c", "<p>a <strong>b</strong> c</p>"},
		{"italic", "a *b* c", "<p>a <em>b</em> c</p>"},
		{"inline code", "use `SELECT 1` here", "<p>use <code>SELECT 1</code> here</p>"},
		{"unordered list", "- one\n- two", "<ul><li>one</li><li>two</li></ul>"},
		{"ordered list", "1. one\n2. two", "<ol><li>one</li><li>two</li></ol>"},
		{"blockquote", "> quoted", "<blockquote><p>quoted</p></blockquote>"},
		{"horizontal rule", "---", "<hr>"},
		{"link", "[docs](https://example.com)",
			`<p><a href="https://example.com" rel="noopener noreferrer">docs</a></p>`},
		{"fenced code", "```sql\nSELECT 1\n```",
			`<pre class="code-block lang-sql"><code>SELECT 1</code></pre>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(Render(tc.in)); got != tc.want {
				t.Errorf("Render(%q)\n got: %s\nwant: %s", tc.in, got, tc.want)
			}
		})
	}
}

// The renderer takes untrusted-ish prose from YAML files, so escaping is not
// optional.
func TestRenderEscapesHTML(t *testing.T) {
	cases := []struct {
		name string
		in   string
		bad  string
	}{
		{"raw script tag", `<script>alert(1)</script>`, "<script>"},
		{"img onerror", `<img src=x onerror=alert(1)>`, "<img"},
		{"javascript link", `[x](javascript:alert(1))`, "javascript:"},
		{"html in code span", "`<b>bold</b>`", "<b>"},
		{"html in fenced block", "```\n<script>x</script>\n```", "<script>"},
		{"html in heading", "# <script>x</script>", "<script>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(Render(tc.in))
			if contains(got, tc.bad) {
				t.Errorf("Render(%q) leaked %q: %s", tc.in, tc.bad, got)
			}
		})
	}
}

func TestRenderNestedList(t *testing.T) {
	got := string(Render("- one\n  - nested\n- two"))
	want := "<ul><li>one</li><ul><li>nested</li></ul><li>two</li></ul>"
	if got != want {
		t.Errorf("\n got: %s\nwant: %s", got, want)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
