Vendored browser dependencies (no CDN at runtime):

- htmx 2.0.11: https://cdn.jsdelivr.net/npm/htmx.org@2.0.11/dist/htmx.min.js
  License: https://raw.githubusercontent.com/bigskysoftware/htmx/v2.0.11/LICENSE
- htmx-ext-sse 2.2.4: https://cdn.jsdelivr.net/npm/htmx-ext-sse@2.2.4/sse.js
  License: https://cdn.jsdelivr.net/npm/htmx-ext-sse@2.2.4/LICENSE
- IBM Plex Sans 400/500/600 and IBM Plex Mono 400/500, Latin subset, from Fontsource 5.3.0:
  https://cdn.jsdelivr.net/npm/@fontsource/ibm-plex-sans@5.3.0/files/ibm-plex-sans-latin-400-normal.woff2
  (and the same path for the other weights and for @fontsource/ibm-plex-mono@5.3.0)
  License: SIL Open Font License 1.1, copied to fonts/IBM-Plex-Sans-LICENSE and fonts/IBM-Plex-Mono-LICENSE

htmx and the SSE extension are 0BSD. The adjacent license files are copied verbatim from upstream.
Run `shasum -a 256 htmx-2.0.11.min.js sse-2.2.4.js fonts/*.woff2` to check the pinned files:
d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717  internal/web/static/htmx-2.0.11.min.js
3b5992a541619babefc4c169505af474df5c3039da51e59b96ccf9241ecd61d2  internal/web/static/sse-2.2.4.js
08949f728dc52d528e69b1667d15c89a5686a4ee9a296ff90983985f99c380f7  internal/web/static/fonts/ibm-plex-mono-latin-400-normal.woff2
01d285447409c8a588692162439a038b8cbd7871309ee20267b0d2d91c6e8e22  internal/web/static/fonts/ibm-plex-mono-latin-500-normal.woff2
3b646991d30055a93a4ecc499713d4347953a74a947ecab435ab72070cbdab0e  internal/web/static/fonts/ibm-plex-sans-latin-400-normal.woff2
0717336fb31fcdcde4b8deb3675bb4a0f7f6d484864afcd6751ac29975962203  internal/web/static/fonts/ibm-plex-sans-latin-500-normal.woff2
8960851d691c054ed38e259bdcf1a6190d157b4203ed5bb32c632a863fb8ec2f  internal/web/static/fonts/ibm-plex-sans-latin-600-normal.woff2
