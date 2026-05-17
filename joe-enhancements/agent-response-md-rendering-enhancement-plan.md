# Agent Response Markdown Rendering — Enhancement Plan

## Problem

The LLM is instructed to use markdown (tables, bold, bullets, headers) but the
chat UI renders its responses as plain text. The root cause is two compounding
issues:

1. **Go's `html/template` HTML-escapes the bot string** — `**bold**` renders
   literally instead of as `<strong>bold</strong>`.
2. **`.msg` CSS has `white-space: pre-wrap`** — correct for user messages but
   wrong for bot responses that contain rendered HTML elements, where `<p>` and
   `<table>` tags should control spacing.

Affected file: `web/main.go:577` (botTpl definition) and `web/templates/index.html`.

---

## Proposed Solution

Server-side markdown → HTML conversion using
[`github.com/yuin/goldmark`](https://github.com/yuin/goldmark), the most
actively maintained Go markdown library. The HTML fragment returned from
`/chat` will already contain proper `<strong>`, `<table>`, `<code>`, `<ul>`
etc. elements. No client-side JS library is needed, keeping the HTMX/no-build
philosophy intact.

---

## Changes Required

### 1. Add goldmark dependency — `web/go.mod`

```
go get github.com/yuin/goldmark
```

Goldmark is CommonMark-compliant, extensible, and has zero transitive
dependencies.

### 2. Convert markdown to HTML in the render path — `web/main.go`

**Current (`main.go:577` and `:583`):**
```go
botTpl = template.Must(template.New("b").Parse(`<div class="msg bot">{{.}}</div>`))

func renderBot(w http.ResponseWriter, s string) { botTpl.Execute(w, s) }
```

**After:**
```go
var md = goldmark.New(
    goldmark.WithExtensions(extension.Table, extension.Strikethrough),
    goldmark.WithRendererOptions(html.WithUnsafe()),
)

botTpl = template.Must(template.New("b").Parse(`<div class="msg bot">{{.}}</div>`))

func renderBot(w http.ResponseWriter, s string) {
    var buf bytes.Buffer
    if err := md.Convert([]byte(s), &buf); err != nil {
        buf.WriteString(template.HTMLEscapeString(s))
    }
    botTpl.Execute(w, template.HTML(buf.String()))
}
```

`template.HTML(...)` tells Go's template engine the string is already safe HTML
and must not be escaped again. The Table and Strikethrough goldmark extensions
cover the formatting the LLM uses most.

### 3. Scope `white-space: pre-wrap` — `web/templates/index.html`

**Current:**
```css
.msg { ... white-space: pre-wrap; }
```

**After:**
```css
.msg        { ... }                      /* remove pre-wrap from base */
.msg.user   { white-space: pre-wrap; }  /* keep for user bubbles */
.msg.tool   { white-space: pre-wrap; }  /* keep for tool trace lines */
```

Bot responses will use normal block layout; newlines inside code blocks are
handled by `<pre>` tags goldmark emits.

### 4. Add CSS for rendered markdown elements — `web/templates/index.html`

Add inside the existing `<style>` block:

```css
/* Markdown rendered inside bot bubbles */
.msg.bot p                        { margin: 0 0 0.5rem; }
.msg.bot p:last-child             { margin-bottom: 0; }
.msg.bot ul, .msg.bot ol          { margin: 0 0 0.5rem 1.25rem; padding: 0; }
.msg.bot li                       { margin-bottom: 0.2rem; }
.msg.bot table                    { border-collapse: collapse; width: 100%; margin-bottom: 0.5rem; font-size: 0.88rem; }
.msg.bot th, .msg.bot td          { border: 1px solid #d0d0d0; padding: 0.3rem 0.6rem; text-align: left; }
.msg.bot th                       { background: #f0f0f0; font-weight: 600; }
.msg.bot code                     { background: #f0f0f0; padding: 0.1em 0.3em; border-radius: 3px; font-size: 0.85em; font-family: ui-monospace, monospace; }
.msg.bot pre                      { background: #f0f0f0; padding: 0.65rem; border-radius: 0.4rem; overflow-x: auto; margin-bottom: 0.5rem; }
.msg.bot pre code                 { background: none; padding: 0; }
.msg.bot h1,h2,h3                 { margin: 0.6rem 0 0.3rem; font-size: 1rem; }
.msg.bot strong                   { font-weight: 600; }
.msg.bot blockquote               { margin: 0 0 0.5rem 0; padding-left: 0.75rem; border-left: 3px solid #ccc; color: #555; }
```

---

## Files Changed

| File | Change |
|---|---|
| `web/go.mod` + `web/go.sum` | Add `github.com/yuin/goldmark` |
| `web/main.go` | Add goldmark instance; update `renderBot` to convert + pass `template.HTML` |
| `web/templates/index.html` | Scope `white-space: pre-wrap`; add markdown element styles |

No other files need to change. The MCP server path does not render HTML so it
is unaffected.

---

## What Renders Correctly After This Change

| Markdown | Before | After |
|---|---|---|
| `**bold**` | `**bold**` | **bold** |
| ` \| col1 \| col2 \|` table | raw pipes | rendered table |
| `- bullet` list | `- bullet` | • bullet |
| ` \`inline code\` ` | backticks | highlighted span |
| ` ```block``` ` | raw fence | styled pre block |
| `# Header` | `# Header` | heading |

---

## Out of Scope

- Syntax highlighting inside code blocks (would require a JS library or a
  server-side highlighter — not needed for this data's output style)
- Streaming / partial rendering (current architecture sends the full response
  after the agent loop completes — no change needed)
- User message bubbles (intentionally kept as plain text / pre-wrap)
