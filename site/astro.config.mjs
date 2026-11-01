// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

import { base, repo, site } from './site.config.mjs';

export default defineConfig({
	site,
	base,
	integrations: [
		starlight({
			title: 'Einsora',
			description:
				'A free, open-source GPU cloud you run yourself: notebooks, OpenAI-compatible model endpoints, and GPU VMs on a Mac, a Linux box, or a datacenter. MIT licensed.',
			logo: { src: './src/assets/logo.svg' },
			favicon: '/favicon.svg',
			social: [{ icon: 'github', label: 'GitHub', href: repo }],
			// Pages made from the repository's Markdown set their own edit link, to their source file.
			editLink: { baseUrl: `${repo}/edit/main/site/` },
			customCss: ['./src/styles/custom.css'],
			components: { MarkdownContent: './src/components/MarkdownContent.astro' },
			head: [
				{ tag: 'meta', attrs: { property: 'og:image:alt', content: 'Einsora: your own GPU cloud' } },
				{ tag: 'meta', attrs: { name: 'theme-color', content: '#4f46e5' } },
			],
			sidebar: [
				{
					label: 'Start here',
					items: [
						{ label: 'Why Einsora', slug: 'why' },
						{ label: 'Quickstart', slug: 'runbooks/00-quickstart' },
						{ label: 'Product definition', slug: 'product/definition' },
						{ label: 'Roadmap', slug: 'product/roadmap' },
						{ label: 'Glossary', slug: 'product/glossary' },
					],
				},
				{ label: 'Architecture', items: [{ autogenerate: { directory: 'architecture' } }] },
				{ label: 'Decisions (ADRs)', collapsed: true, items: [{ autogenerate: { directory: 'decisions' } }] },
				{ label: 'Runbooks', collapsed: true, items: [{ autogenerate: { directory: 'runbooks' } }] },
				{ label: 'Components', items: [{ autogenerate: { directory: 'components' } }] },
				{
					label: 'Contributing',
					collapsed: true,
					items: [
						{ label: 'How to contribute', slug: 'contributing' },
						{ label: 'Instructions for AI agents', slug: 'contributing/agents' },
						{ label: 'Rules per component', collapsed: true, items: [{ autogenerate: { directory: 'contributing/rules' } }] },
						{ label: 'Playbooks', collapsed: true, items: [{ autogenerate: { directory: 'contributing/playbooks' } }] },
						{ label: 'ADR template', slug: 'contributing/adr-template' },
					],
				},
				{ label: 'Hardware', collapsed: true, items: [{ autogenerate: { directory: 'hardware' } }] },
				{
					label: 'Project',
					items: [
						{ label: 'License (MIT)', slug: 'license' },
						{ label: 'Security', slug: 'project/security' },
						{ label: 'README', slug: 'project/readme' },
						{ label: 'Documentation map', slug: 'project/docs' },
					],
				},
			],
		}),
	],
});
