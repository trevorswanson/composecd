Vendored browser dependencies (no CDN at runtime):

- htmx 2.0.11: https://cdn.jsdelivr.net/npm/htmx.org@2.0.11/dist/htmx.min.js
  License: https://raw.githubusercontent.com/bigskysoftware/htmx/v2.0.11/LICENSE
- htmx-ext-sse 2.2.4: https://cdn.jsdelivr.net/npm/htmx-ext-sse@2.2.4/sse.js
  License: https://cdn.jsdelivr.net/npm/htmx-ext-sse@2.2.4/LICENSE

Both are 0BSD. The adjacent license files are copied verbatim from upstream.
Run `shasum -a 256 htmx-2.0.11.min.js sse-2.2.4.js` to check the pinned files:
d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717  internal/web/static/htmx-2.0.11.min.js
3b5992a541619babefc4c169505af474df5c3039da51e59b96ccf9241ecd61d2  internal/web/static/sse-2.2.4.js
