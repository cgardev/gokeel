# gokeel documentation site

The documentation site for [gokeel](https://github.com/cgardev/gokeel), built
with [Astro](https://docs.astro.build) and
[Starlight](https://starlight.astro.build) and published as a GitHub Pages
project site at <https://cgardev.github.io/gokeel/>.

## Project structure

```
docs/
├── public/                     Static assets served as-is (favicon).
├── src/
│   ├── assets/                 Images imported by pages (header logos).
│   ├── content/
│   │   └── docs/               The English pages — the canonical content.
│   │       ├── index.mdx       Landing page.
│   │       ├── getting-started.md
│   │       ├── guides/         One guide per module or concern.
│   │       ├── reference/      One reference page per module.
│   │       ├── cookbook/       Task-oriented recipes.
│   │       └── es/             Spanish translations (mirrors the tree above).
│   ├── content.config.ts       Content collection wiring for Starlight.
│   └── styles/                 The monochrome palette over the Ion theme.
├── astro.config.mjs            Site, sidebar, locales, and plugin configuration.
└── package.json
```

Every `.md` or `.mdx` file under `src/content/docs/` becomes a route based on
its path. The sidebar is declared explicitly in `astro.config.mjs`, so a new
page must also be added there.

## Translations

The site is configured for page-by-page translation. English is the **root
locale**: the canonical pages live directly under `src/content/docs/` and keep
their URLs (`/gokeel/guides/event-bus/`). Each additional language is declared
in the `locales` map of `astro.config.mjs` and lives in a directory named after
its BCP-47 tag, mirroring the English tree exactly:

```
src/content/docs/guides/event-bus.md        →  /gokeel/guides/event-bus/
src/content/docs/es/guides/event-bus.md     →  /gokeel/es/guides/event-bus/
```

The rules of the workflow:

- **Translate one page at a time.** A page with no translation in a locale
  automatically falls back to the English content, shown with a
  "not yet translated" notice and the language picker, so a locale never
  breaks the build by being incomplete.
- **Never change file names or paths.** The path relative to the locale
  directory is the page's identity; renaming it creates a different page
  instead of a translation.
- **Translate frontmatter values, not keys.** `title` and `description` are
  translated; the field names and any slugs stay as they are.
- **Adjust internal links to the locale.** A translated page links to its
  siblings under the locale path (`/gokeel/es/guides/...`), falling back to
  the English path only for pages that intentionally have no translation.
- **Code blocks stay in English.** Identifiers, comments, and output in code
  samples follow the repository's English-only code standard; only the prose
  around them is translated.
- **Sidebar labels** are translated in `astro.config.mjs` by adding a
  `translations` map next to each `label`, for example
  `{ label: 'Guides', translations: { es: 'Guías' } }`. Starlight ships its
  own user-interface strings (search, table of contents, the fallback notice)
  for the configured languages, so those need no work.

To add a new language, declare it in the `locales` map and create its
directory under `src/content/docs/`; everything else follows from the rules
above.

Machine-assisted translations follow
[`TRANSLATION_PROMPT.md`](TRANSLATION_PROMPT.md): point an agent (for
example the Gemini CLI) at that file and name the pages to translate, and it
reads the sources, writes the translated files into the locale tree, and
verifies the build. Its output is a draft that a human reviews page by page.

## Commands

All commands run from the `docs/` directory:

| Command        | Action                                             |
| :------------- | :------------------------------------------------- |
| `pnpm install` | Install dependencies.                               |
| `pnpm dev`     | Start the local development server.                 |
| `pnpm build`   | Build the production site into `./dist/`.           |
| `pnpm preview` | Preview the production build locally.               |
