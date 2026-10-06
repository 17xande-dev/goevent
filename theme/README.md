# theme/

Your theme. The compose stack mounts this directory into the server and sets
`TEMPLATE_DIR=/theme/templates` and `STATIC_DIR=/theme/static`, with
`THEME_RELOAD=true` — so a file dropped in here takes effect on the next page
refresh, with no restart and no rebuild.

- `templates/` — shaped like
  [`internal/handler/templates/`](../internal/handler/templates): `layouts/`,
  `partials/`, `pages/`, `admin/`, `mail/`. A file at the **same path** as an
  embedded default replaces the definitions in it; every other definition, and
  every other file, keeps falling back. The path is the contract, so a
  `not_found.gohtml` at the top of this directory is a file nothing looks for.
- `static/` — assets by name, flat. A `styles.css` here shadows the bundled one; a
  `logo.svg` here rebrands the header. New names are served too, so an overridden
  template can reference its own `hero.png`.

Each page is parsed into a template set of its own — the shared partials, its
layout, then the page file. That is what lets one page here fill the layout's
`nav_extra` block on that page and nowhere else. It also means a
page can only call a partial, its own layout, or something it defines itself:
anything two pages need belongs in `partials/`.

Both directories start empty, which means the site looks exactly as it does with
no theme at all. Copy a default out of
[`internal/handler/templates/`](../internal/handler/templates) or
[`internal/handler/static/`](../internal/handler/static) — keeping its
subdirectory — and edit the copy.

Copy only what you are actually changing. A copied file that is not being changed
is a copy that silently stops receiving fixes: it goes on rendering the version of
that page you took, and a later release that corrects it changes nothing for you.

Nothing in here is used unless `TEMPLATE_DIR` / `STATIC_DIR` point at it, so this
directory is safe to leave empty, delete the contents of, or keep in a branch of
your own.
