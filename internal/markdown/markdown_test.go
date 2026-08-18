package markdown

import (
	"strings"
	"testing"
)

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

func TestTable(t *testing.T) {
	got := string(Render("| a | b |\n|---|---|\n| 1 | 2 |"))
	want := `<table class="md-table"><thead><tr><th>a</th><th>b</th></tr></thead>` +
		`<tbody><tr><td>1</td><td>2</td></tr></tbody></table>`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestTableAlignment(t *testing.T) {
	got := string(Render("| l | c | r |\n|:--|:-:|--:|\n| 1 | 2 | 3 |"))
	for _, want := range []string{
		`<th>l</th>`,
		`<th class="md-center">c</th>`,
		`<th class="md-right">r</th>`,
		`<td class="md-right">3</td>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}

// The delimiter row is bare dashes, which is also a horizontal rule. The header
// row is what tells them apart.
func TestHorizontalRuleIsNotATable(t *testing.T) {
	got := string(Render("some prose\n\n---\n\nmore prose"))
	if !strings.Contains(got, "<hr>") {
		t.Errorf("expected a horizontal rule, got %s", got)
	}
	if strings.Contains(got, "<table") {
		t.Errorf("a rule became a table: %s", got)
	}
}

// A line of pipes with no delimiter row under it is ordinary prose.
func TestPipesWithoutDelimiterAreProse(t *testing.T) {
	got := string(Render("a | b | c"))
	if strings.Contains(got, "<table") {
		t.Errorf("expected prose, got %s", got)
	}
}

func TestTableWithoutOuterPipes(t *testing.T) {
	got := string(Render("a | b\n--- | ---\n1 | 2"))
	if !strings.Contains(got, "<th>a</th><th>b</th>") {
		t.Errorf("expected two headers, got %s", got)
	}
	if !strings.Contains(got, "<td>1</td><td>2</td>") {
		t.Errorf("expected two cells, got %s", got)
	}
}

// A short row is padded rather than left ragged, so one miscounted row cannot
// break the table's shape.
func TestTableRaggedRow(t *testing.T) {
	got := string(Render("| a | b | c |\n|---|---|---|\n| 1 |\n| 1 | 2 | 3 | 4 |"))
	if !strings.Contains(got, "<tr><td>1</td><td></td><td></td></tr>") {
		t.Errorf("short row not padded: %s", got)
	}
	if !strings.Contains(got, "<tr><td>1</td><td>2</td><td>3</td></tr>") {
		t.Errorf("long row not truncated: %s", got)
	}
}

func TestTableCellsGetInlineFormatting(t *testing.T) {
	got := string(Render("| a | b |\n|---|---|\n| `X,GAP` | **bold** |"))
	if !strings.Contains(got, "<code>X,GAP</code>") {
		t.Errorf("code span not rendered: %s", got)
	}
	if !strings.Contains(got, "<strong>bold</strong>") {
		t.Errorf("bold not rendered: %s", got)
	}
}

func TestTableCellsAreEscaped(t *testing.T) {
	got := string(Render("| a |\n|---|\n| <script>x</script> |"))
	if strings.Contains(got, "<script>") {
		t.Errorf("HTML leaked through a table cell: %s", got)
	}
}

func TestTableEscapedPipe(t *testing.T) {
	got := string(Render("| a | b |\n|---|---|\n| x \\| y | z |"))
	if !strings.Contains(got, "<td>x | y</td>") {
		t.Errorf("escaped pipe not honoured: %s", got)
	}
}
