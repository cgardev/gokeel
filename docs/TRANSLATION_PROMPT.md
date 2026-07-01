# Translation instructions

This document instructs an AI agent to translate pages of this documentation
site. It is written for the Gemini CLI, but any agent that can read and write
files in the repository can follow it. To use it, point the agent at this
file and name the pages, for example:

```sh
gemini "Read docs/TRANSLATION_PROMPT.md and follow it: translate
docs/src/content/docs/guides/event-bus.md and
docs/src/content/docs/guides/sql-bus.md to Spanish"
```

Pages can be named one by one, as a directory ("all the guides"), or as the
whole site. The output of the agent is a draft: a human reviews every
translated page before it is committed.

---

You are translating the documentation of gokeel, a family of Go libraries
for modular monoliths (a transaction manager, event buses, a transactional
outbox), published with Astro Starlight from `docs/src/content/docs/`. The
request that brought you here names the pages to translate and the target
language. Everything below is binding.

## Workflow

1. **Resolve the target locale.** Map the requested language to the locale
   directory declared in the `locales` map of `docs/astro.config.mjs`
   (Spanish → `es`). If the language is not declared there, stop and report
   it instead of inventing a directory.
2. **Resolve the pages.** Expand the request to concrete `.md` and `.mdx`
   files under `docs/src/content/docs/`, never including locale directories
   themselves (they hold translations, not sources).
3. **Translate each page** following the contract below, one file at a time.
4. **Write each translation** to `docs/src/content/docs/<locale>/` at the
   same relative path with the same file name, creating directories as
   needed. If the target file already exists, leave it untouched and report
   it — an existing translation may already be human-reviewed — unless the
   request explicitly says to overwrite.
5. **Modify nothing else.** The English sources, `astro.config.mjs`, and
   every other file stay untouched.
6. **Verify and report.** Run `pnpm run build` inside `docs/` and confirm it
   succeeds, then list the files you wrote and any page you skipped and why.

## The translation contract

Treat every page as a high-stakes document, the way a sworn translator
treats a contract: readers will design and operate production systems from
your words. A translation error that changes a guarantee, inverts a
negation, or alters a default value is a defect with real consequences.
Fidelity of meaning is the only acceptable outcome; elegance never
justifies drift.

### What you must NEVER translate

Reproduce the following byte for byte, exactly as they appear in the source:

- Fenced code blocks (``` ... ```), in their entirety: code, comments,
  strings, and command output inside them stay in English, untouched.
- Inline code spans (`LikeThis`), including identifiers, types, functions,
  methods, package names, column names, statuses, flags, and values.
- Commands, file paths, directory names, and URLs.
- YAML frontmatter keys and any slug or identifier-like value.
- Proper names of products, projects, and patterns: gokeel, Go, PostgreSQL,
  SQLite, Spring, Spring Modulith, Starlight, GitHub, and Go module import
  paths.

If you are unsure whether a fragment is code or prose, treat it as code and
leave it untouched.

### What you must translate

- All running prose: paragraphs, headings, list items, table cells that are
  not code, image alternative text, and admonition text.
- In the frontmatter, only the VALUES of `title` and `description`.

### Fidelity rules (the legal standard)

- Translate sentence for sentence. Never add, omit, merge, summarize, or
  reorder content. Never add translator notes, footnotes, or explanations.
- Preserve modality exactly: "must", "should", "may", "never", "can", and
  "cannot" each map to their precise counterpart in the target language;
  never strengthen or weaken an obligation or a permission.
- Preserve delivery-guarantee vocabulary exactly. Terms such as
  "at-least-once", "exactly once", and "once per node" define system
  behavior: translate them the same way every time, and where the target
  language has no established equivalent, keep the English term and add the
  translation in parentheses on its first occurrence in the page.
- Numbers, durations, defaults, versions, and units pass through unchanged.
- Translate what is written, not what you believe should have been written.
  If the source seems wrong, translate it faithfully anyway and mention the
  suspicion in your final report, never inside the page.

### Naturalness rules

- Be literal in meaning, not in syntax: write each sentence the way a native
  technical writer of the target language would express exactly that
  meaning. Avoid word-for-word calques; restructure the grammar whenever the
  target language requires it, without altering the meaning.
- Use one consistent translation for each recurring term across the whole
  page and across pages, matching the glossary below. When a technical term
  has no established, unambiguous equivalent, keep the English term as a
  loanword rather than inventing a translation.

### Structure rules

- The output is the same Markdown document, translated: identical heading
  hierarchy, list structure, tables, emphasis, blockquotes, and blank-line
  layout. Do not convert between Markdown constructs.
- Keep the frontmatter delimiters and keys exactly as in the source; the
  YAML must remain valid.
- Internal documentation links: a target that starts with `/gokeel/` gets
  the locale inserted after it — `/gokeel/guides/event-bus/` becomes
  `/gokeel/es/guides/event-bus/` for Spanish. Do not modify asset links, the
  `/gokeel/llms.txt` link, or external URLs. Translate the link TEXT, never
  the link path itself beyond that locale insertion.
- Keep fragment anchors (`#...`) as they are.

### Glossary for Spanish (es)

Apply when the target language is Spanish; for other languages, derive an
equivalent glossary from the same criteria and keep it consistent.

Keep in English (established loanwords in Spanish technical writing):
commit, rollback, listener, outbox, backoff, polling, claim (noun), lease,
dead letter, wake signal, frontier.

Translate consistently:

- event bus → bus de eventos
- delivery → entrega
- unit of work → unidad de trabajo
- retention → retención
- grace (window/period) → (ventana/período de) gracia
- attempt → intento
- node → nodo
- cluster-wide → en todo el clúster
- competing/broadcast (delivery modes) → keep the English mode name and
  gloss it in Spanish on first use, for example "competing (competitivo)".

### Self-check per page

Before writing each file, verify every point; if any fails, fix it and check
again:

1. Every fenced code block is byte-identical to the source, and the count
   of code blocks matches.
2. Every inline code span is unchanged.
3. The heading count and levels match the source.
4. The frontmatter keys are unchanged and only the `title` and
   `description` values are translated.
5. Every internal `/gokeel/...` documentation link carries the locale
   segment; no other link was modified.
6. No source sentence was dropped, added, or merged.
7. No English prose remains outside code, names, and glossary loanwords.
8. Every "must/should/may/never" and every delivery guarantee reads with
   exactly the strength of the original.
9. The file contains only the translated document — no commentary, no
   markers, nothing before the opening `---` of the frontmatter.
