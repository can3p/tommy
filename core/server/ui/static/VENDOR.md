# Vendored assets

Committed, not fetched at runtime or build time: tommy ships as a single binary
with no network dependency, and a CDN hotlink would break offline use and leak
the fact that someone is testing.

| File | Source | Version | License |
|---|---|---|---|
| `htmx.min.js` | https://cdnjs.cloudflare.com/ajax/libs/htmx/2.0.4/htmx.min.js | 2.0.4 | BSD-2-Clause |
| `htmx-ext-sse.js` | https://cdn.jsdelivr.net/npm/htmx-ext-sse@2.2.3/sse.js | 2.2.3 | BSD-2-Clause |

`app.css`, `app.js` and `favicon.svg` are ours.
