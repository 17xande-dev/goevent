# Theming

A theme is two directories: templates that override the embedded ones by path, and assets
that override the bundled ones by name. Nothing is forked or rebuilt, and anything you do
not override keeps coming from the binary.

| | Points at | Overrides |
|---|---|---|
| `TEMPLATE_DIR` | a directory shaped like [`internal/handler/templates/`](../internal/handler/templates) | templates, by path: `pages/event.gohtml` replaces `pages/event.gohtml` |
| `STATIC_DIR` | a flat directory of assets | bundled files, by file name |

`make up` and `make run` already point them at [`theme/templates`](../theme) and
[`theme/static`](../theme), with **`THEME_RELOAD=true`**, so writing a theme is editing a
file and refreshing the page. Both directories are empty on a clean checkout.

## Writing one

Start from a default rather than from nothing:

```sh
cp internal/handler/static/styles.css theme/static/styles.css     # restyle
mkdir -p theme/templates/pages                                    # the path is the contract
cp internal/handler/templates/pages/event.gohtml theme/templates/pages/
```

Then edit and refresh. Deleting your copy puts the default back. Keep the subdirectory: an
`event.gohtml` at the top of `TEMPLATE_DIR` is a file nothing looks for, and nothing warns
about it.

Copy only what you are changing. A copied file stops receiving fixes: it goes on rendering
the version you took.

**The CSS is the intended level to work at.** Every colour, size and spacing value in
[`styles.css`](../internal/handler/static/styles.css) is a custom property in one `:root`
block at the top, and no colour literal appears below it:

```css
/* theme/static/styles.css, after copying the default and editing its :root */
:root {
  --paper: #fffdf8;  --paper-sunk: #f4efe4;
  --ink: #241f18;    --ink-soft: #5c5348;  --ink-faint: #8d8375;
  --rule: #e2d9c8;
  --accent: #7a2e1f; --accent-ink: #fffdf8;  /* buttons and links */
  --warn: #8a2f1d;
  --font: "Iowan Old Style", Georgia, serif;
  --radius: 0;       --page: 1000px;
}
```

The full set is `--paper`, `--paper-sunk`, `--ink`, `--ink-soft`, `--ink-faint`, `--rule`,
`--accent`, `--accent-ink`, `--warn`; `--font`, `--font-mono`, `--text`, `--text-small`,
`--text-large`, `--title`, `--title-page`; `--gap-xs`, `--gap-sm`, `--gap`, `--gap-md`,
`--gap-lg`, `--gap-xl`; `--radius`, `--radius-lg`, `--measure`, `--page`.

A `styles.css` in `STATIC_DIR` **replaces** the bundled one; there is no cascade between
them. So copy the whole default and edit its `:root`, or write your own from scratch. Keep
the `.htmx-indicator` rules if you write your own (see `partials/document.gohtml`).

**No inline styles.** The CSP has no `'unsafe-inline'`, so a `style="…"` attribute, a
`<style>` block, an `onclick=` or an inline `<script>` in a template is silently ignored by
the browser. Put CSS in a stylesheet and JavaScript in a `.js` file in `STATIC_DIR`,
referenced with `{{asset "yours.js"}}`. See [Security](security.md#response-headers).

## Web fonts

The default theme uses the system font stack: no download, and no third-party request on
the registration page. Two ways to change that.

**Self-host.** Drop the `.woff2` into `STATIC_DIR` and reference it from your stylesheet
with a `/static/...` URL. It is served from this origin, so the CSP already allows it.

**Use a hosted service** with two variables:

```sh
FONT_ORIGINS=https://use.typekit.net,https://p.typekit.net
FONT_CSS_URL=https://use.typekit.net/abc1def.css
```

`FONT_CSS_URL` is linked from every page's `<head>`. `FONT_ORIGINS` widens **two** CSP
directives, `style-src` for the stylesheet and `font-src` for the files it names. Typekit
serves those from two hosts, so a kit that loads and still renders in the fallback font
usually means the second host is missing. Google Fonts is the same shape, with
`https://fonts.googleapis.com,https://fonts.gstatic.com`. `FONT_CSS_URL` without its origin
in `FONT_ORIGINS` refuses to boot.

Linking a font makes it available; set `--font` in your `styles.css` to use it. Only a CSS
embed works: a font service's JavaScript loader needs `script-src` widened, which this
project does not do. A hosted font also lets the font CDN see your visitors' IP addresses.

## Overriding templates

Overriding is per path: a file at `pages/events.gohtml` under `TEMPLATE_DIR` replaces the
definitions in the embedded `pages/events.gohtml` and leaves everything else alone.

| Directory | Holds | Wrapped in |
|---|---|---|
| `layouts/` | `public.gohtml`, `admin.gohtml`, each defining `layout` | — |
| `partials/` | pieces parsed into **every** page: `document_head`, `csrf`, `err`, `question`, `event_fields`, the status badges, the account menu, `checkout_status_block`, `error_reference` | — |
| `pages/` | the public site, registration and checkout, error pages, and the admin sign-in and setup pages | `layouts/public.gohtml` |
| `admin/` | everything behind the admin login | `layouts/admin.gohtml` |
| `mail/` | the email bodies | nothing; a message is not a page |

**Each page is parsed into a template set of its own**: the partials, its layout, then the
page file ([`internal/handler/templates.go`](../internal/handler/templates.go)). A
definition in one page file reaches that page and no other. The other side of that: a page
can only call a partial, its own layout, or something it defines itself. Anything two pages
need belongs in `partials/`.

**A page file defines `content`**, and the layout renders it; a page that defines none
refuses the boot. The public layout has one more block, `nav_extra`, an addition to the
site header for a page that wants one, empty by default. The admin layout has `content`
only.

The pages:

| File | Is |
|---|---|
| `pages/events.gohtml` | The list of upcoming events (`/` redirects here) |
| `pages/event.gohtml` | One event's page |
| `pages/register.gohtml` | The registration form. Must keep the hidden `checkout_key` field, which stops a resubmitted form booking twice |
| `pages/checkout_redirect.gohtml` | The hand-over to a gateway: a form posted on load (PayFast) or a link and QR code (SnapScan) |
| `pages/checkout_return.gohtml` | Where a gateway sends the browser back |
| `pages/checkout_status.gohtml` | The QR hand-over's status, also defining the `checkout_status_fragment` htmx polls |
| `pages/registration.gohtml` | The registrant's manage page, with their tickets once confirmed |
| `pages/not_found.gohtml`, `pages/error_client.gohtml`, `pages/error_server.gohtml` | 404, other 4xx, and 5xx. See [Operations](operations.md#when-something-goes-wrong) |
| `pages/admin_login.gohtml`, `pages/admin_setup.gohtml` | Sign-in and first-account setup, in the public layout |
| `admin/admin_*.gohtml` | The admin. `admin_checkin.gohtml` also defines the polled `checkin_counts` fragment; `admin_account.gohtml` defines `account_menu` |
| `partials/document.gohtml` | Everything from the doctype to `</head>` for both layouts. Two htmx settings in it are load-bearing; keep them |
| `mail/*` | See [Email templates](email.md#email-templates) |

A page's file name is the name handlers render it by, in one namespace across `pages/`,
`admin/` and `mail/`, which is why the admin files keep their `admin_` prefix.

Things to know before writing one:

- **Class names are the contract** between templates and stylesheet. Change the markup and
  keep the names, or change both together.
- **Every form needs `{{template "csrf" .CSRFToken}}`**, or it gets a `403`. See
  [CSRF](security.md#csrf).
- **Templates get exactly the data the handler passes.** Every page has `.Title`,
  `.SiteName`, `.Currency`, `.CSRFToken`, `.BaseURL`, `.FontCSSURL` and `.User` (the
  signed-in administrator, if any), plus its own (`.Event`,
  `.Registration`, …). Templates can also ask `.Can "permission"` to hide controls. The
  functions are `money` (cents to a displayed amount), `asset` (a bundled or overridden file
  to its content-hashed URL), `image` (an event's image key to where it is served),
  `linebreaks` and `paragraphs` (typed text to HTML, escaped first), `dict` (build a map to
  pass a partial several values) and `label` (a stored word such as `eft` or `payfast` to
  its display form).
- **A field or template name that does not exist is a `500` on that page**, not a refused
  boot: Go checks both when a template runs. Render every page you have touched before
  shipping a theme.
- **`layouts/public.gohtml` carries the account menu slot**,
  `<span class="account-slot" hx-get="/admin/account/menu" …>`, inside its header nav. A
  copied layout needs to keep it for signed-in administrators to see their menu.

## Using a theme in a deployment

`deploy/standard` and `deploy/tunnel` both mount a `theme/` directory next to
`compose.yaml` into the server at `/theme`, read-only. The server runs in a container, so
`TEMPLATE_DIR` and `STATIC_DIR` are paths **inside it**:

```sh
cd /opt/goevent
sudo git clone https://github.com/you/your-theme.git theme    # or: mkdir theme
```

Then uncomment these in `.env`:

```sh
TEMPLATE_DIR=/theme/templates
STATIC_DIR=/theme/static
```

and `sudo docker compose up -d`. Leave `THEME_RELOAD` unset.

- **The mount is always there.** With neither variable set nothing reads it, and if
  `theme/` does not exist Docker creates it empty.
- **Set a variable only when its directory exists.** A `TEMPLATE_DIR` or `STATIC_DIR` that
  is missing, or not a directory, refuses to boot. A host path such as `./theme/static`
  does not exist inside the container.
- **The files must be world-readable**, since the server runs as an unprivileged user. A
  `git clone` gives you that.
- **A change needs a restart**: `sudo docker compose restart server`. A template that no
  longer parses refuses the boot rather than serving a broken page.

## Reloading, and not reloading

`THEME_RELOAD=true` re-reads both directories on **every request**. It is for writing a
theme and nothing else:

- It reparses every template and re-reads every asset per request.
- It turns a file saved half-written while the server runs into a `500` on whichever page
  uses it.

A theme that is already broken at startup fails the boot either way: both directories are
validated before anything is served. The server logs a warning at startup when reloading is
on.

Asset URLs carry a hash of the file's contents (`/static/styles.css?v=…`) and are served
`immutable`, so a replaced file is a new URL and a refresh is enough.

## Bundled assets

Shipped in the binary ([`internal/handler/static`](../internal/handler/static)):
`styles.css`, vendored htmx, `redirect.js` (submits the PayFast hand-over form, since an
inline script would be blocked), `logo.svg` and `placeholder.svg`. Override any of them
with a file of the same name in `STATIC_DIR`; dropping in a `logo.svg` rebrands the header.
New names are served too, so an overridden template can reference its own `hero.png`.

The default logo has no text, because the name comes from `SITE_NAME`.

**Only these extensions are served**: `.js`, `.css`, `.svg`, `.png`, `.jpg`, `.jpeg`,
`.gif`, `.webp`, `.ico`, `.woff2`. A note, an `.html` or a `.php` left in `STATIC_DIR` is
not published.

Event images are not assets. They are uploaded in the admin and stored in `IMAGE_DIR` or
the bucket, never in `STATIC_DIR`.
