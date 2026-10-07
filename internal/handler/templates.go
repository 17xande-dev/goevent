// Package handler renders the site's HTML: full pages and the fragments htmx
// swaps into them.
package handler

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	texttemplate "text/template"

	"github.com/17xande-dev/goevent/internal/blob"
	"github.com/17xande-dev/goevent/internal/events"
)

//go:embed templates
var templatesFS embed.FS

// Where each kind of template lives, and what wraps it. The directory is the whole
// convention: it decides which layout a page gets, so nothing in a page file has to
// name one and no page can be wrapped in two.
//
//   - layouts/  one file per layout, each defining "layout" — which is why they can
//     never share a set, and why this is a per-page-set design rather than one set
//     with a flag on it.
//   - partials/ parsed into every set: the <head>, the CSRF field, the question fields.
//   - pages/    the public event pages and the sign-in page, wrapped in layouts/public.
//   - admin/    everything behind RequireAdmin, wrapped in layouts/admin.
//   - mail/     rendered with no layout at all; a message is not a page.
const (
	layoutsDir  = "templates/layouts"
	partialsDir = "templates/partials"
	mailDir     = "templates/mail"
)

// pageDirs maps a directory of pages to the layout file that wraps them, "" meaning
// none. Ordered rather than a map so that a name collision is reported the same way
// twice running.
var pageDirs = []struct{ dir, layout string }{
	{"templates/pages", "public"},
	{"templates/admin", "admin"},
	{"templates/mail", ""},
}

// A page's file name is the name handlers render it by: templates/pages/event.gohtml
// is "event". That is why the admin files keep their admin_ prefix — the names live
// in one flat namespace as far as a handler is concerned, and a second events.gohtml
// under admin/ would be a boot-time collision rather than a helpful shorthand.

// target is where one renderable name lives: which set holds it, and which template
// in that set to execute. A page resolves to its set's "layout"; a fragment — the
// htmx half of the same page — resolves to itself, with no layout around it.
type target struct {
	set  *template.Template
	name string
}

// Templates is the parsed theme: one template set per page, each built from the
// shared partials, that page's layout, and the page file itself, with same-named
// files from an override directory layered on top of each.
//
// One set per page is what makes a definition local. A "nav_extra" in
// pages/events.gohtml overrides the empty block in layouts/public.gohtml on the
// event list and on no other page, because no other page's set ever sees that file.
// The cost is that a name is no longer visible everywhere: a page can only call a
// partial, its own layout, or something it defines itself, and a call to anything
// else is a failed render rather than a parse error — Go resolves template names at
// execute time. Rendering every page is therefore a test that earns its keep; see
// templates_test.go.
//
// The text set is separate and stays flat, because email needs both halves of a
// message and they must not be escaped the same way. Pages and HTML mail parts go
// through html/template, which escapes; the plain-text mail part goes through
// text/template, which does not — running a confirmation through the HTML escaper
// would put `&amp;` in front of a registrant.
type Templates struct {
	// mu guards the sets, which reload replaces on the fly.
	mu     sync.RWMutex
	byName map[string]target
	text   *texttemplate.Template

	// What a reload needs to parse the sets again. Kept here rather than passed in,
	// because a render is the only thing that triggers one and it has neither.
	overrideDir string
	images      blob.Storage
	reload      bool
}

// ParseTemplates parses the embedded defaults and then, when overrideDir is set,
// re-parses same-*pathed* files from that directory over them — a later definition
// of a template name replaces an earlier one, and the path is what pairs an
// override with the default it replaces. Adopters restyle the site by dropping
// files into a directory shaped like templates/ instead of forking the project.
//
// A file under a path the defaults do not have is ignored rather than refused: a
// theme directory holding a note, a backup or a work in progress is an ordinary
// thing, and refusing to boot over one would be worse than doing nothing with it.
//
// Overrides are read at startup, so changing a file needs a restart but never a
// rebuild — unless SetReload has been called, which is what the development
// stack does so that a refresh is enough.
//
// images resolves an event's image key to the URL it is served at. It is the
// storage backend, so a template can render an image without the row having
// recorded where the bytes happen to live today.
func ParseTemplates(overrideDir string, images blob.Storage) (*Templates, error) {
	tmpl := &Templates{overrideDir: overrideDir, images: images}
	if err := tmpl.parse(); err != nil {
		return nil, err
	}
	return tmpl, nil
}

// SetReload makes every render re-read the override directory first, so editing a
// template and refreshing the page shows the change without a restart.
//
// Development only, and not because of taste: it reparses every template on every
// request, and it moves a syntax error in an override from a boot failure to a 500
// on whichever page happens to use it. Call it before serving; the flag is read
// without synchronisation, unlike the template set it governs.
func (t *Templates) SetReload(on bool) { t.reload = on }

// parse builds every set from the embedded defaults with the override directory
// layered on top, and installs them. It replaces them only on success, so a reload
// that hits a half-saved override leaves the last good ones in place and fails that
// request rather than emptying the theme.
//
// The order it builds in is the mechanism, not an implementation detail. Partials
// first, then a layout on top of them, then the page on top of that — because a
// later definition of a name replaces an earlier one, and the whole point is that a
// page's "content" replaces the layout's empty block rather than the other way
// round.
func (t *Templates) parse() error {
	common, err := template.New("goevent").Funcs(funcs(t.images)).ParseFS(templatesFS, partialsDir+"/*.gohtml")
	if err != nil {
		return fmt.Errorf("handler: parse partials: %w", err)
	}
	if common, err = t.override(common, partialsDir); err != nil {
		return err
	}

	layouts, err := t.parseLayouts(common)
	if err != nil {
		return err
	}

	byName := map[string]target{}
	for _, pd := range pageDirs {
		base := common
		if pd.layout != "" {
			base = layouts[pd.layout]
			if base == nil {
				return fmt.Errorf("handler: %s has no layouts/%s.gohtml", pd.dir, pd.layout)
			}
		}
		if err := t.parsePages(byName, base, common, pd.dir, pd.layout != ""); err != nil {
			return err
		}
	}

	text, err := texttemplate.New("goevent").Funcs(funcs(t.images)).ParseFS(templatesFS, mailDir+"/*.txt")
	if err != nil {
		return fmt.Errorf("handler: parse embedded text templates: %w", err)
	}
	if t.overrideDir != "" {
		matches, err := filepath.Glob(filepath.Join(t.overrideDir, "mail", "*.txt"))
		if err != nil {
			return fmt.Errorf("handler: scan TEMPLATE_DIR for mail/*.txt: %w", err)
		}
		if len(matches) > 0 {
			if text, err = text.ParseFiles(matches...); err != nil {
				return fmt.Errorf("handler: parse override text templates: %w", err)
			}
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.byName, t.text = byName, text
	return nil
}

// parseLayouts builds one base set per file in layouts/: the partials, with that
// layout on top. They are separate sets because each defines "layout", so a second
// one parsed into the same set would simply replace the first.
func (t *Templates) parseLayouts(common *template.Template) (map[string]*template.Template, error) {
	files, err := fs.ReadDir(templatesFS, layoutsDir)
	if err != nil {
		return nil, fmt.Errorf("handler: read %s: %w", layoutsDir, err)
	}
	layouts := map[string]*template.Template{}
	for _, f := range files {
		name, ok := templateName(f)
		if !ok {
			continue
		}
		base, err := common.Clone()
		if err != nil {
			return nil, fmt.Errorf("handler: clone partials for %s: %w", name, err)
		}
		if base, err = t.parseFile(base, path.Join(layoutsDir, f.Name())); err != nil {
			return nil, err
		}
		if base.Lookup("layout") == nil {
			return nil, fmt.Errorf("handler: %s/%s defines no \"layout\"", layoutsDir, f.Name())
		}
		layouts[name] = base
	}
	return layouts, nil
}

// parsePages builds one set per file in dir and records every name it can be
// rendered by.
//
// Each file is parsed twice, and the second parse is what makes the first worth
// doing: parsing it over the bare partials says which names *this file* introduced,
// which is both the list to register and the assertion that a laid-out page defines
// "content" at all. Without it a page that forgot would render an empty shell — the
// one failure the block form allows — and it would do so with a 200.
func (t *Templates) parsePages(byName map[string]target, base, common *template.Template, dir string, laidOut bool) error {
	files, err := fs.ReadDir(templatesFS, dir)
	if err != nil {
		return fmt.Errorf("handler: read %s: %w", dir, err)
	}
	for _, f := range files {
		page, ok := templateName(f)
		if !ok {
			continue
		}
		file := path.Join(dir, f.Name())

		alone, err := common.Clone()
		if err != nil {
			return fmt.Errorf("handler: clone partials for %s: %w", file, err)
		}
		if alone, err = t.parseFile(alone, file); err != nil {
			return err
		}
		if laidOut && alone.Lookup("content") == nil {
			return fmt.Errorf("handler: %s defines no \"content\", so it would render an empty page", file)
		}

		set, err := base.Clone()
		if err != nil {
			return fmt.Errorf("handler: clone layout for %s: %w", file, err)
		}
		if set, err = t.parseFile(set, file); err != nil {
			return err
		}

		// Everything the file added over the bare partials, less three kinds of name
		// nothing renders directly: the set's own root, the templates Go names after
		// the files themselves — those hold only the whitespace between this
		// project's {{define}} blocks — and anything the layout or a partial already
		// defines, which covers "content", "nav_extra", and a page that restyles a
		// partial for itself.
		for _, own := range alone.Templates() {
			name := own.Name()
			if name == "goevent" || strings.Contains(name, ".") || base.Lookup(name) != nil {
				continue
			}
			if err := claim(byName, name, target{set: set, name: name}, file); err != nil {
				return err
			}
		}
		if laidOut {
			if err := claim(byName, page, target{set: set, name: "layout"}, file); err != nil {
				return err
			}
		}
	}
	return nil
}

// claim records one renderable name, refusing a second file that wants it. Two
// pages defining one fragment name used to be legal and silent — the last file
// parsed won — and a per-page set turns it into something worth saying out loud,
// because the two definitions would now behave differently depending on which page
// was being rendered.
func claim(byName map[string]target, name string, tgt target, file string) error {
	if _, taken := byName[name]; taken {
		return fmt.Errorf("handler: %s defines %q, which another file already defines", file, name)
	}
	byName[name] = tgt
	return nil
}

// parseFile parses one embedded file into set, then the adopter's file of the same
// path under TEMPLATE_DIR when there is one — so the override's definitions replace
// the defaults it names and leave the rest alone.
func (t *Templates) parseFile(set *template.Template, file string) (*template.Template, error) {
	set, err := set.ParseFS(templatesFS, file)
	if err != nil {
		return nil, fmt.Errorf("handler: parse %s: %w", file, err)
	}
	if t.overrideDir == "" {
		return set, nil
	}
	override := filepath.Join(t.overrideDir, filepath.FromSlash(strings.TrimPrefix(file, "templates/")))
	if _, err := os.Stat(override); err != nil {
		// Not there is the ordinary case: an adopter overriding one page should not
		// have to supply an email template as well.
		return set, nil
	}
	if set, err = set.ParseFiles(override); err != nil {
		return nil, fmt.Errorf("handler: parse override %s: %w", override, err)
	}
	return set, nil
}

// override layers every override for a whole directory of partials or layouts.
func (t *Templates) override(set *template.Template, dir string) (*template.Template, error) {
	files, err := fs.ReadDir(templatesFS, dir)
	if err != nil {
		return nil, fmt.Errorf("handler: read %s: %w", dir, err)
	}
	for _, f := range files {
		if _, ok := templateName(f); !ok {
			continue
		}
		if set, err = t.parseFile(set, path.Join(dir, f.Name())); err != nil {
			return nil, err
		}
	}
	return set, nil
}

// templateName is the name a file contributes: its base name without the
// extension. Anything that is not a .gohtml file is skipped, which is how the .txt
// mail templates stay out of the HTML sets.
func templateName(f fs.DirEntry) (string, bool) {
	if f.IsDir() || !strings.HasSuffix(f.Name(), ".gohtml") {
		return "", false
	}
	return strings.TrimSuffix(f.Name(), ".gohtml"), true
}

// current returns the sets, reparsing them first when reloading is on. Both are
// returned together because a reload replaces both, and a caller holding one from
// before it and one from after would be reading two different themes.
func (t *Templates) current() (map[string]target, *texttemplate.Template, error) {
	if t.reload {
		if err := t.parse(); err != nil {
			return nil, nil, err
		}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.byName, t.text, nil
}

func funcs(images blob.Storage) template.FuncMap {
	return template.FuncMap{
		// Amounts are cents everywhere in Go; the decimal point exists only in
		// the rendered page.
		"money": events.FormatPrice,
		// asset resolves a bundled file to its content-addressed URL.
		"asset": assetURL,
		// image resolves an event's image key to where it is served from. A
		// function rather than a field on the event, so it cannot be forgotten:
		// there is nothing for a handler to populate and therefore nothing to leave
		// unpopulated.
		"image": func(key string) string {
			if key == "" {
				return ""
			}
			return images.URL(key)
		},
		// linebreaks renders multi-line text — an address, typed into a textarea —
		// as HTML.
		"linebreaks": linebreaks,
		// paragraphs renders prose typed into a textarea — an event description —
		// as HTML paragraphs.
		"paragraphs": paragraphs,
		// dict builds a map from alternating keys and values, for calling a
		// partial that needs more than one thing: html/template passes one value.
		"dict": dict,
		// label is a stored word — a payment method, an email kind, a payment
		// status — for a person. An unknown word is shown as it is.
		"label": func(s string) string {
			if l, ok := labels[s]; ok {
				return l
			}
			return s
		},
	}
}

// labels are the stored words the admin shows, and how it shows them.
var labels = map[string]string{
	"eft": "EFT", "cash": "Cash", "payfast": "PayFast", "snapscan": "SnapScan",
	"pending": "Pending", "paid": "Paid", "failed": "Failed", "cancelled": "Cancelled",
	"confirmation": "Confirmation with tickets", "received": "Payment instructions",
	"notify": "Organiser's copy", "resend": "Tickets sent again",
}

func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, fmt.Errorf("dict: odd number of arguments")
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v is not a string", kv[i])
		}
		m[k] = kv[i+1]
	}
	return m, nil
}

// paragraphs escapes text and renders it as <p> elements: a blank line between
// two blocks of text separates paragraphs, and a single newline within one is a
// line break. linebreaks alone would turn a blank line into two <br>s, which
// looks like a paragraph and is not one. Safe as template.HTML for the same
// reason linebreaks is: each block is escaped before any tag is added.
func paragraphs(s string) template.HTML {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	for _, block := range blankLine.Split(s, -1) {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		b.WriteString("<p>")
		b.WriteString(string(linebreaks(block)))
		b.WriteString("</p>")
	}
	return template.HTML(b.String())
}

// blankLine is a paragraph break: a newline, then a line holding nothing but
// whitespace, then another newline.
var blankLine = regexp.MustCompile(`\n[ \t]*\n`)

// linebreaks escapes text and then turns its newlines into <br>, in that order.
// Escaping first is what makes returning template.HTML safe here: the only markup
// in the result is the tags this function put there.
func linebreaks(s string) template.HTML {
	escaped := template.HTMLEscapeString(s)
	// Textareas submit CRLF, so collapse that before looking for bare newlines or
	// every line would gain a stray carriage return.
	escaped = strings.ReplaceAll(escaped, "\r\n", "\n")
	return template.HTML(strings.ReplaceAll(escaped, "\n", "<br>"))
}

// Execute renders one named template to bytes, writing nothing. Splitting this
// out of Render is what lets a caller turn a template failure into a 500: nothing
// has been sent at the point it returns an error, so the status is still the
// caller's to choose.
func (t *Templates) Execute(name string, data any) ([]byte, error) {
	byName, _, err := t.current()
	if err != nil {
		return nil, err
	}
	return executeIn(byName, name, data)
}

// executeIn finds the set a name lives in and executes it there.
//
// A page renders its layout, which calls back into the page; a fragment renders
// itself, with no layout around it. That is the same split the handlers already
// make — fragmentOr picks the fragment for an htmx request and the page for a
// visit — and it is why one lookup answers both.
func executeIn(byName map[string]target, name string, data any) ([]byte, error) {
	tgt, ok := byName[name]
	if !ok {
		return nil, fmt.Errorf("handler: render %s: no such template", name)
	}
	var buf bytes.Buffer
	if err := tgt.set.ExecuteTemplate(&buf, tgt.name, data); err != nil {
		return nil, fmt.Errorf("handler: render %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// Render executes one named template and writes it. It renders into a buffer
// first, so a template error becomes the caller's problem to answer rather than a
// half-written page that already claimed 200.
func (t *Templates) Render(w http.ResponseWriter, status int, name string, data any) error {
	body, err := t.Execute(name, data)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, err = w.Write(body)
	return err
}

// String executes one named HTML template and returns it, for an email body
// rather than a response.
func (t *Templates) String(name string, data any) (string, error) {
	byName, _, err := t.current()
	if err != nil {
		return "", err
	}
	body, err := executeIn(byName, name, data)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// Text executes one named text template — the plain-text half of an email.
func (t *Templates) Text(name string, data any) (string, error) {
	_, text, err := t.current()
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := text.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("handler: render text %s: %w", name, err)
	}
	return buf.String(), nil
}
