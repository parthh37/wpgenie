package mailer

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"text/template"
)

// Templates are plain text with two conventions, so that one source gives
// both the text and the HTML part:
//
//   - a blank line separates paragraphs;
//   - a line "[[Label|https://…]]" is a button (in the text part:
//     "Label: https://…").
//
// URLs in the text become links. Subject and body are Go text/templates
// ({{.Invoice.Number}}); every template also gets {{.Brand.Name}},
// {{.Brand.URL}} and {{.PanelURL}}.

// Template is a message's default subject and body.
type Template struct {
	Name        string `json:"name"`        // "invoice.created"
	Group       string `json:"group"`       // "Billing", "Support"…
	Description string `json:"description"` // when it's sent
	Subject     string `json:"subject"`
	Body        string `json:"body"`
	// Vars documents the placeholders for the editor ("Invoice.Number").
	Vars []string `json:"vars"`
	// Sample is data the preview renders with.
	Sample map[string]any `json:"-"`
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Template{}
)

// Register adds a template's default (from a package's init). Registering
// a name twice panics: two features can't share a message.
func Register(t Template) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[t.Name]; dup {
		panic("mailer: template registered twice: " + t.Name)
	}
	if _, err := parse(t.Name, t.Subject, t.Body); err != nil {
		panic("mailer: template " + t.Name + ": " + err.Error())
	}
	registry[t.Name] = t
}

// Registered returns every template's default, by group then name.
func Registered() []Template {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := slices.Collect(maps.Values(registry))
	slices.SortFunc(out, func(a, b Template) int {
		if c := strings.Compare(a.Group, b.Group); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

func lookup(name string) (Template, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	t, ok := registry[name]
	return t, ok
}

// Override is staff's version of a template.
type Override struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

func overrideKey(name string) string { return "mail_template:" + name }

// TemplateView is a template as the editor shows it: the text in use and
// the default.
type TemplateView struct {
	Template
	DefaultSubject string `json:"default_subject"`
	DefaultBody    string `json:"default_body"`
	Customized     bool   `json:"customized"`
}

// Templates lists every template with its current text.
func (s *Service) Templates(ctx context.Context) ([]TemplateView, error) {
	out := []TemplateView{}
	for _, t := range Registered() {
		v, err := s.view(ctx, t)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, nil
}

func (s *Service) view(ctx context.Context, t Template) (*TemplateView, error) {
	v := &TemplateView{Template: t, DefaultSubject: t.Subject, DefaultBody: t.Body}
	o, err := s.override(ctx, t.Name)
	if err != nil {
		return nil, err
	}
	if o != nil {
		v.Subject, v.Body, v.Customized = o.Subject, o.Body, true
	}
	return v, nil
}

func (s *Service) override(ctx context.Context, name string) (*Override, error) {
	raw, err := s.Store.Setting(ctx, overrideKey(name))
	if err != nil || raw == "" {
		return nil, err
	}
	var o Override
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// SetTemplate stores staff's version of a template, after checking that it
// renders with the template's sample data.
func (s *Service) SetTemplate(ctx context.Context, name string, o Override) (*TemplateView, error) {
	t, ok := lookup(name)
	if !ok {
		return nil, fmt.Errorf("%w: no template %q", ErrInvalid, name)
	}
	if strings.TrimSpace(o.Subject) == "" || strings.TrimSpace(o.Body) == "" {
		return nil, fmt.Errorf("%w: subject and body are required", ErrInvalid)
	}
	if len(o.Subject) > 500 || len(o.Body) > 20000 {
		return nil, fmt.Errorf("%w: template too long", ErrInvalid)
	}
	if _, err := s.renderWith(ctx, t, o.Subject, o.Body, t.Sample); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	b, err := json.Marshal(o)
	if err != nil {
		return nil, err
	}
	if err := s.Store.SetSetting(ctx, overrideKey(name), string(b)); err != nil {
		return nil, err
	}
	return s.view(ctx, t)
}

// ResetTemplate goes back to the default.
func (s *Service) ResetTemplate(ctx context.Context, name string) (*TemplateView, error) {
	t, ok := lookup(name)
	if !ok {
		return nil, fmt.Errorf("%w: no template %q", ErrInvalid, name)
	}
	if err := s.Store.SetSetting(ctx, overrideKey(name), ""); err != nil {
		return nil, err
	}
	return s.view(ctx, t)
}

// Preview renders a subject and body (the stored ones when both are "")
// with the template's sample data.
func (s *Service) Preview(ctx context.Context, name string, o Override) (*Rendered, error) {
	t, ok := lookup(name)
	if !ok {
		return nil, fmt.Errorf("%w: no template %q", ErrInvalid, name)
	}
	if o.Subject == "" && o.Body == "" {
		v, err := s.view(ctx, t)
		if err != nil {
			return nil, err
		}
		o = Override{Subject: v.Subject, Body: v.Body}
	}
	r, err := s.renderWith(ctx, t, o.Subject, o.Body, t.Sample)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return r, nil
}

// Rendered is a message ready to send.
type Rendered struct {
	Subject string `json:"subject"`
	Text    string `json:"text"`
	HTML    string `json:"html"`
}

// Render fills a template (staff's version if there is one) with data.
// A customized template that no longer renders falls back to the default:
// a typo in the editor must not stop invoices going out.
func (s *Service) Render(ctx context.Context, name string, data map[string]any) (*Rendered, error) {
	t, ok := lookup(name)
	if !ok {
		return nil, fmt.Errorf("%w: no template %q", ErrInvalid, name)
	}
	o, err := s.override(ctx, name)
	if err != nil {
		return nil, err
	}
	if o != nil {
		r, err := s.renderWith(ctx, t, o.Subject, o.Body, data)
		if err == nil {
			return r, nil
		}
		s.Log.Warn("mailer: customized template failed; using the default", "template", name, "err", err)
	}
	return s.renderWith(ctx, t, t.Subject, t.Body, data)
}

func (s *Service) renderWith(ctx context.Context, t Template, subject, body string, data map[string]any) (*Rendered, error) {
	tpl, err := parse(t.Name, subject, body)
	if err != nil {
		return nil, err
	}
	brand := s.brand(ctx)
	d := map[string]any{"Brand": brand, "PanelURL": s.PanelURL}
	maps.Copy(d, data)
	var subj, text strings.Builder
	if err := tpl.ExecuteTemplate(&subj, "subject", d); err != nil {
		return nil, err
	}
	if err := tpl.ExecuteTemplate(&text, "body", d); err != nil {
		return nil, err
	}
	return layout(brand, oneLine(noValue(subj.String()), 250), noValue(text.String())), nil
}

func parse(name, subject, body string) (*template.Template, error) {
	tpl := template.New(name).Option("missingkey=zero")
	if _, err := tpl.New("subject").Parse(subject); err != nil {
		return nil, fmt.Errorf("subject: %w", err)
	}
	if _, err := tpl.New("body").Parse(body); err != nil {
		return nil, fmt.Errorf("body: %w", err)
	}
	return tpl, nil
}

// noValue blanks what text/template prints for a missing map key.
func noValue(s string) string { return strings.ReplaceAll(s, "<no value>", "") }

var (
	buttonRe = regexp.MustCompile(`^\[\[([^|\]]{1,80})\|(https?://[^\s\]]+)\]\]$`)
	urlRe    = regexp.MustCompile(`https?://[^\s<>"']+[^\s<>"'.,;:!?)]`)
)

// layout turns the text body into the text and HTML parts.
func layout(b Brand, subject, body string) *Rendered {
	body = strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	var text, htm strings.Builder
	for i, para := range splitParagraphs(body) {
		if i > 0 {
			text.WriteString("\n\n")
		}
		lines := strings.Split(para, "\n")
		if len(lines) == 1 {
			if m := buttonRe.FindStringSubmatch(strings.TrimSpace(lines[0])); m != nil {
				text.WriteString(m[1] + ": " + m[2])
				fmt.Fprintf(&htm, `<p style="margin:24px 0"><a href="%s" style="display:inline-block;background:#4f46e5;color:#ffffff;`+
					`text-decoration:none;padding:12px 22px;border-radius:8px;font-weight:600">%s</a></p>`,
					html.EscapeString(m[2]), html.EscapeString(m[1]))
				continue
			}
		}
		text.WriteString(para)
		htm.WriteString(`<p style="margin:0 0 16px">`)
		for j, l := range lines {
			if j > 0 {
				htm.WriteString("<br>")
			}
			htm.WriteString(linkify(l))
		}
		htm.WriteString("</p>")
	}
	if b.Footer != "" {
		text.WriteString("\n\n--\n" + b.Name + "\n" + b.Footer)
	}
	return &Rendered{Subject: subject, Text: text.String() + "\n", HTML: wrapHTML(b, subject, htm.String())}
}

func splitParagraphs(s string) []string {
	var out []string
	for _, p := range regexp.MustCompile(`\n[ \t]*\n+`).Split(s, -1) {
		if p = strings.Trim(p, "\n"); strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// linkify escapes a line and turns its URLs into links.
func linkify(line string) string {
	var b strings.Builder
	last := 0
	for _, loc := range urlRe.FindAllStringIndex(line, -1) {
		b.WriteString(html.EscapeString(line[last:loc[0]]))
		u := html.EscapeString(line[loc[0]:loc[1]])
		fmt.Fprintf(&b, `<a href="%s" style="color:#4f46e5">%s</a>`, u, u)
		last = loc[1]
	}
	b.WriteString(html.EscapeString(line[last:]))
	return b.String()
}

func wrapHTML(b Brand, subject, content string) string {
	head := html.EscapeString(b.Name)
	if b.LogoURL != "" {
		head = fmt.Sprintf(`<img src="%s" alt="%s" style="max-height:40px;max-width:200px">`, html.EscapeString(b.LogoURL), head)
	}
	if b.URL != "" {
		head = fmt.Sprintf(`<a href="%s" style="color:#111827;text-decoration:none">%s</a>`, html.EscapeString(b.URL), head)
	}
	footer := ""
	if b.Footer != "" {
		footer = strings.ReplaceAll(html.EscapeString(b.Footer), "\n", "<br>")
	}
	return `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">` +
		`<title>` + html.EscapeString(subject) + `</title></head>` +
		`<body style="margin:0;background:#f3f4f6;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;` +
		`color:#111827;font-size:15px;line-height:1.55">` +
		`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#f3f4f6;padding:24px 12px"><tr><td align="center">` +
		`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:600px">` +
		`<tr><td style="padding:8px 4px 16px;font-size:18px;font-weight:700">` + head + `</td></tr>` +
		`<tr><td style="background:#ffffff;border-radius:12px;padding:28px 28px 12px;border:1px solid #e5e7eb">` + content + `</td></tr>` +
		`<tr><td style="padding:16px 4px;color:#6b7280;font-size:12px">` + footer + `</td></tr>` +
		`</table></td></tr></table></body></html>`
}
