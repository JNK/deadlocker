// Package markdown renders the small subset of Markdown that scenario
// descriptions actually use.
//
// Pulling in a full CommonMark implementation would be the largest dependency
// in the project by a wide margin, for prose we control. This handles
// headings, paragraphs, fenced and inline code, bold, italic, links, blockquotes,
// horizontal rules and both kinds of list.
//
// Safety: the input is HTML-escaped before any markup is inserted, so a
// description can never inject tags. Link targets are restricted to http, https
// and relative URLs.
package markdown

import (
	"html/template"
	"regexp"
	"strings"
)

var (
	reHeading   = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reULItem    = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	reOLItem    = regexp.MustCompile(`^(\s*)\d+[.)]\s+(.*)$`)
	reQuote     = regexp.MustCompile(`^>\s?(.*)$`)
	reBold      = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reItalic    = regexp.MustCompile(`(^|[\s(])\*([^*\n]+)\*($|[\s).,;:!?])`)
	reLink      = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	reAutoLink  = regexp.MustCompile(`(^|\s)(https?://[^\s<>()]+)`)
	reSafeURL   = regexp.MustCompile(`^(https?://|/|#|\./|\.\./)`)
	reCodeFence = regexp.MustCompile("^\\s*```\\s*(\\w*)\\s*$")
)

// Render converts markdown source to HTML.
func Render(src string) template.HTML {
	var out strings.Builder
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")

	// lists tracks open <ul>/<ol> elements by indentation so nested lists close
	// in the right order.
	var lists []listLevel
	inQuote := false
	var para []string

	closeLists := func(toIndent int) {
		for len(lists) > 0 && lists[len(lists)-1].indent >= toIndent {
			if lists[len(lists)-1].ordered {
				out.WriteString("</ol>")
			} else {
				out.WriteString("</ul>")
			}
			lists = lists[:len(lists)-1]
		}
	}
	flushPara := func() {
		if len(para) == 0 {
			return
		}
		out.WriteString("<p>")
		out.WriteString(inline(strings.Join(para, " ")))
		out.WriteString("</p>")
		para = nil
	}
	closeQuote := func() {
		if inQuote {
			out.WriteString("</blockquote>")
			inQuote = false
		}
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]

		// Fenced code block: consume verbatim until the closing fence.
		if m := reCodeFence.FindStringSubmatch(line); m != nil {
			flushPara()
			closeLists(0)
			closeQuote()
			lang := m[1]
			var body []string
			i++
			for i < len(lines) && !reCodeFence.MatchString(lines[i]) {
				body = append(body, lines[i])
				i++
			}
			cls := "code-block"
			if lang != "" {
				cls += " lang-" + escape(lang)
			}
			out.WriteString(`<pre class="` + cls + `"><code>`)
			out.WriteString(escape(strings.Join(body, "\n")))
			out.WriteString("</code></pre>")
			continue
		}

		if strings.TrimSpace(line) == "" {
			flushPara()
			closeLists(0)
			closeQuote()
			continue
		}

		if isHorizontalRule(line) {
			flushPara()
			closeLists(0)
			closeQuote()
			out.WriteString("<hr>")
			continue
		}

		if m := reHeading.FindStringSubmatch(line); m != nil {
			flushPara()
			closeLists(0)
			closeQuote()
			level := len(m[1])
			// Descriptions sit inside a panel that already has a heading, so
			// shift everything down one level to keep the document outline sane.
			if level < 6 {
				level++
			}
			tag := "h" + string(rune('0'+level))
			out.WriteString("<" + tag + ">" + inline(m[2]) + "</" + tag + ">")
			continue
		}

		if m := reQuote.FindStringSubmatch(line); m != nil {
			flushPara()
			closeLists(0)
			if !inQuote {
				out.WriteString("<blockquote>")
				inQuote = true
			}
			out.WriteString("<p>" + inline(m[1]) + "</p>")
			continue
		}

		if m := reULItem.FindStringSubmatch(line); m != nil {
			flushPara()
			closeQuote()
			openListItem(&out, &lists, len(m[1]), false, m[2])
			continue
		}
		if m := reOLItem.FindStringSubmatch(line); m != nil {
			flushPara()
			closeQuote()
			openListItem(&out, &lists, len(m[1]), true, m[2])
			continue
		}

		// A plain line inside an open list continues the current item.
		if len(lists) > 0 && strings.HasPrefix(line, "  ") {
			out.WriteString(" " + inline(strings.TrimSpace(line)))
			continue
		}

		closeLists(0)
		para = append(para, strings.TrimSpace(line))
	}

	flushPara()
	closeLists(0)
	closeQuote()
	return template.HTML(out.String())
}

type listLevel struct {
	indent  int
	ordered bool
}

func openListItem(out *strings.Builder, lists *[]listLevel, indent int, ordered bool, text string) {
	for len(*lists) > 0 && (*lists)[len(*lists)-1].indent > indent {
		if (*lists)[len(*lists)-1].ordered {
			out.WriteString("</ol>")
		} else {
			out.WriteString("</ul>")
		}
		*lists = (*lists)[:len(*lists)-1]
	}
	if len(*lists) == 0 || (*lists)[len(*lists)-1].indent < indent {
		if ordered {
			out.WriteString("<ol>")
		} else {
			out.WriteString("<ul>")
		}
		*lists = append(*lists, listLevel{indent: indent, ordered: ordered})
	}
	out.WriteString("<li>" + inline(text) + "</li>")
}

// codeMark stands in for an extracted code span while emphasis and links are
// applied. It is a private-use rune rather than a control character because
// html/template's escaper rewrites NULL to U+FFFD, which would destroy it.
const codeMark = ""

// inline applies span-level formatting. Code spans are pulled out first so
// their contents are never reinterpreted as emphasis or links.
func inline(s string) string {
	// Strip any pre-existing sentinel so input can never desynchronise the
	// placeholder bookkeeping.
	s = strings.ReplaceAll(s, codeMark, "")

	var codes []string
	var b strings.Builder
	inCode := false
	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			if inCode {
				inCode = false
				b.WriteString(codeMark)
			} else {
				inCode = true
				codes = append(codes, "")
			}
			continue
		}
		if inCode {
			codes[len(codes)-1] += string(s[i])
			continue
		}
		b.WriteByte(s[i])
	}
	if inCode {
		// Unterminated backtick: treat it as literal text.
		last := codes[len(codes)-1]
		codes = codes[:len(codes)-1]
		b.WriteString("`" + last)
	}

	text := escape(b.String())
	text = reLink.ReplaceAllStringFunc(text, func(m string) string {
		parts := reLink.FindStringSubmatch(m)
		label, href := parts[1], parts[2]
		// href was escaped along with everything else; &amp; is fine in an
		// attribute, but reject anything that is not an obviously safe scheme.
		if !reSafeURL.MatchString(href) {
			return label
		}
		return `<a href="` + href + `" rel="noopener noreferrer">` + label + `</a>`
	})
	text = reAutoLink.ReplaceAllString(text, `$1<a href="$2" rel="noopener noreferrer">$2</a>`)
	text = reBold.ReplaceAllString(text, "<strong>$1</strong>")
	text = reItalic.ReplaceAllString(text, "$1<em>$2</em>$3")

	// Restore code spans in order.
	for _, c := range codes {
		text = strings.Replace(text, codeMark, "<code>"+escape(c)+"</code>", 1)
	}
	return text
}

// isHorizontalRule reports whether a line is three or more of the same rule
// character, optionally spaced. RE2 has no backreferences, so this cannot be a
// regexp.
func isHorizontalRule(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	marker := t[0]
	if marker != '-' && marker != '*' && marker != '_' {
		return false
	}
	count := 0
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case marker:
			count++
		case ' ', '\t':
		default:
			return false
		}
	}
	return count >= 3
}

func escape(s string) string {
	return template.HTMLEscapeString(s)
}
