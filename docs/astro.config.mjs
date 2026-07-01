// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import { ion } from 'starlight-ion-theme';
import starlightLlmsTxt from 'starlight-llms-txt';

// https://astro.build/config
export default defineConfig({
	// The site is published as a GitHub Pages project site at
	// https://cgardev.github.io/gokeel/, so the origin and the base path
	// are configured separately.
	site: 'https://cgardev.github.io',
	base: '/gokeel',
	integrations: [
		starlight({
			title: 'gokeel',
			description:
				'Building blocks for a modular monolith in Go: a declarative transaction manager, an in-process event bus, a transactional outbox, hierarchical log-level management, and externalized configuration.',
			// Apply the Ion theme, recolored with the monochrome
			// palette defined in ./src/styles/theme.css. useCustomECTheme:false lets
			// code blocks follow the same palette instead of Ion's built-in theme.
			// starlightLlmsTxt generates llms.txt, llms-full.txt, and llms-small.txt
			// at build time so language models can consume the documentation. It
			// reads the Astro base path, so the files are served under /gokeel.
			plugins: [
				ion({
					useCustomECTheme: false,
					// Surface the generated llms.txt index in the footer. Ion renders
					// the href verbatim, so it includes the /gokeel base path.
					footer: {
						links: [{ label: 'llms.txt', href: '/gokeel/llms.txt' }],
					},
				}),
				starlightLlmsTxt({
					projectName: 'gokeel',
					description:
						'Building blocks for a modular monolith in Go, inspired by Spring and Spring Modulith: a context-bound declarative transaction manager with propagation and commit synchronizations, a synchronous in-process event bus, a transactional outbox that publishes events after commit, hierarchical log-level management over log/slog, and externalized configuration from JSON documents with environment placeholders. The transaction, eventbus, logging, and conf cores depend only on the Go standard library; the outbox adds a pluggable schema migrator.',
				}),
			],
			customCss: ['./src/styles/theme.css'],
			// The header logo and the browser tab icon both use the keel artwork.
			// The header logo ships as two fixed-colour files so it tracks the
			// Starlight theme toggle exactly; the favicon is a single file that
			// follows the operating system colour scheme. Both are served from the
			// project: the logos relative to the root, the favicon from public/.
			logo: {
				light: './src/assets/keel-light.svg',
				dark: './src/assets/keel-dark.svg',
				alt: 'gokeel',
				replacesTitle: false,
			},
			favicon: '/keel.svg',
			social: [
				{
					icon: 'github',
					label: 'GitHub',
					href: 'https://github.com/cgardev/gokeel',
				},
			],
			// Surface an "Edit page" link pointing at the documentation sources
			// within the repository.
			editLink: {
				baseUrl: 'https://github.com/cgardev/gokeel/edit/main/docs/',
			},
			sidebar: [
				{
					label: 'Start Here',
					items: [
						{ label: 'Introduction', link: '/' },
						{ label: 'Getting Started', slug: 'getting-started' },
					],
				},
				{
					label: 'Guides',
					items: [
						{ label: 'Transactions', slug: 'guides/transactions' },
						{
							label: 'Propagation & Synchronizations',
							slug: 'guides/propagation-and-synchronizations',
						},
						{ label: 'The Event Bus', slug: 'guides/event-bus' },
						{
							label: 'The Transactional Outbox',
							slug: 'guides/transactional-outbox',
						},
						{ label: 'The SQL Bus', slug: 'guides/sql-bus' },
						{ label: 'Schema Migrations', slug: 'guides/schema-migrations' },
						{ label: 'Log Levels', slug: 'guides/log-levels' },
						{
							label: 'Externalized Configuration',
							slug: 'guides/externalized-configuration',
						},
					],
				},
				{
					label: 'Reference',
					items: [
						{ label: 'Overview', slug: 'reference' },
						{
							label: 'Transaction Manager',
							slug: 'reference/transaction-manager',
						},
						{
							label: 'Propagation & Options',
							slug: 'reference/propagation-and-options',
						},
						{
							label: 'Synchronizations & Listeners',
							slug: 'reference/synchronizations',
						},
						{ label: 'Event Bus', slug: 'reference/event-bus' },
						{ label: 'Outbox', slug: 'reference/outbox' },
						{ label: 'Schema Migrator', slug: 'reference/migrator' },
						{ label: 'Logging', slug: 'reference/logging' },
						{ label: 'Configuration', slug: 'reference/conf' },
					],
				},
				{
					label: 'Cookbook',
					items: [
						{ label: 'Overview', slug: 'cookbook' },
						{ label: 'Transactional Use Cases', slug: 'cookbook/transactions' },
						{ label: 'In-Process Events', slug: 'cookbook/events' },
						{ label: 'The Outbox', slug: 'cookbook/outbox' },
						{ label: 'Schema Migrations', slug: 'cookbook/migrations' },
						{ label: 'Log Levels', slug: 'cookbook/logging' },
						{ label: 'Configuration', slug: 'cookbook/conf' },
					],
				},
			],
		}),
	],
});
