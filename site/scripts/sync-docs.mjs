// Builds the site's documentation pages from the repository's own Markdown, so the repository stays
// the single source of truth (docs never drift from the code they describe). For each page it:
//
// - takes the title from the first heading, and a description from the first paragraph;
// - links "Edit page" to the source file on GitHub;
// - rewrites links: to another page on the site when the target is one, otherwise to the file or
//   directory on GitHub, and fails on a link to nothing;
// - turns ```mermaid blocks into diagrams drawn in the browser (src/components/MarkdownContent.astro).
//
// Run by `npm run dev` and `npm run build`. Its output is gitignored.

import { existsSync, mkdirSync, readFileSync, readdirSync, statSync, unlinkSync, writeFileSync } from 'node:fs';
import { dirname, join, posix, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { base, repo } from '../site.config.mjs';

const siteDir = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const root = resolve(siteDir, '..');
const out = join(siteDir, 'src/content/docs');
const generated = ['architecture', 'components', 'contributing', 'decisions', 'hardware', 'product', 'project', 'runbooks'];
const prefix = base === '/' ? '' : base.replace(/\/$/, '');

// The pages: repository path → site slug, with optional title, sidebar label, and order.
const pages = new Map();
const add = (path, slug, opts = {}) => pages.set(path, { slug, ...opts });
const md = (dir) => readdirSync(join(root, dir)).filter((f) => f.endsWith('.md')).sort();

add('README.md', 'project/readme', { title: 'README' });
add('docs/README.md', 'project/docs', { title: 'Documentation map' });
add('SECURITY.md', 'project/security');
add('docs/product/product-spec.md', 'product/definition');
add('docs/roadmap.md', 'product/roadmap');
add('docs/glossary.md', 'product/glossary');
add('CONTRIBUTING.md', 'contributing', { order: 0 });
add('AGENTS.md', 'contributing/agents', { title: 'Instructions for AI agents', label: 'AI agents', order: 1 });
for (const [dir, label] of [
	['controlplane', 'Control plane'],
	['ml', 'ML layer'],
	['web', 'Dashboard'],
	['deploy', 'Datacenter deploy'],
	['infra', 'Server bootstrap'],
	['images', 'Images and catalog'],
]) {
	add(`${dir}/README.md`, `components/${dir}`, { label });
	if (existsSync(join(root, dir, 'AGENTS.md'))) {
		add(`${dir}/AGENTS.md`, `contributing/rules/${dir}`, { title: `${label}: rules for contributors and agents`, label });
	}
}
for (const f of md('docs/architecture')) {
	add(`docs/architecture/${f}`, `architecture/${f.replace(/\.md$/, '')}`, f === 'overview.md' ? { order: 0 } : {});
}
const adrLabels = shortADRNames();
for (const f of md('docs/adr')) {
	if (f === 'README.md') add('docs/adr/README.md', 'decisions', { title: 'Architecture decisions', order: 0 });
	else if (f === '0000-template.md') add('docs/adr/0000-template.md', 'contributing/adr-template', { label: 'ADR template' });
	else add(`docs/adr/${f}`, `decisions/${f.replace(/\.md$/, '')}`, { label: adrLabels.get(f.slice(0, 4)) });
}
for (const f of md('docs/runbooks')) {
	add(`docs/runbooks/${f}`, f === 'README.md' ? 'runbooks' : `runbooks/${f.replace(/\.md$/, '')}`, f === 'README.md' ? { order: 0 } : {});
}
for (const f of md('docs/playbooks')) add(`docs/playbooks/${f}`, `contributing/playbooks/${f.replace(/\.md$/, '')}`);
for (const f of md('docs/hardware')) add(`docs/hardware/${f}`, `hardware/${f.replace(/\.md$/, '')}`);

// A link to a directory goes to its index page, when the site has one.
const dirs = new Map([
	['docs/adr', 'decisions'],
	['docs/runbooks', 'runbooks'],
]);

// Section index pages, which live inside their section's directory.
const indexes = new Set(['contributing', 'decisions', 'runbooks']);

// Files are rewritten only when they change, and only stale ones are removed: deleting and
// recreating everything on each run makes file-syncing services (iCloud Drive, Dropbox) leave
// conflict copies behind.
const broken = [];
const written = new Set();
const put = (file, text) => {
	written.add(file);
	mkdirSync(dirname(file), { recursive: true });
	if (!existsSync(file) || readFileSync(file, 'utf8') !== text) writeFileSync(file, text);
};
for (const [path, page] of pages) {
	const file = join(out, indexes.has(page.slug) ? `${page.slug}/index.md` : `${page.slug}.md`);
	put(file, render(path, page, readFileSync(join(root, path), 'utf8')));
}
for (const dir of generated) {
	const walk = (d) => (existsSync(d) ? readdirSync(d, { withFileTypes: true }) : []).flatMap((e) =>
		e.isDirectory() ? walk(join(d, e.name)) : [join(d, e.name)]);
	for (const file of walk(join(out, dir))) if (!written.has(file)) unlinkSync(file);
}
// The license page shows LICENSE itself.
put(join(siteDir, 'src/generated/LICENSE.txt'), readFileSync(join(root, 'LICENSE'), 'utf8'));

if (broken.length) {
	console.error(`sync-docs: ${broken.length} broken link(s):\n${broken.map((b) => `  ${b}`).join('\n')}`);
	process.exit(1);
}
console.log(`sync-docs: ${pages.size} pages from the repository's Markdown`);

function render(path, page, source) {
	if (path === 'docs/adr/0000-template.md') return template(path, page, source);
	const lines = source.replace(/\r\n/g, '\n').split('\n');
	let title = page.title;
	const h1 = lines.findIndex((l) => l.trim() !== '');
	if (h1 >= 0 && /^#\s+/.test(lines[h1])) {
		title ??= lines[h1].replace(/^#\s+/, '').trim();
		lines.splice(h1, 1);
	}
	title ??= posix.basename(path, '.md');
	const body = transform(path, lines);
	const front = [
		'---',
		`title: ${JSON.stringify(title)}`,
		...(describe(lines) ? [`description: ${JSON.stringify(describe(lines))}`] : []),
		`editUrl: ${JSON.stringify(`${repo}/edit/main/${path}`)}`,
		...(page.label || page.order !== undefined
			? ['sidebar:', ...(page.label ? [`  label: ${JSON.stringify(page.label)}`] : []), ...(page.order !== undefined ? [`  order: ${page.order}`] : [])]
			: []),
		'---',
		'',
	];
	return front.join('\n') + body.join('\n');
}

// template shows the ADR template as Markdown to copy: its links are placeholders to fill in.
function template(path, page, source) {
	return [
		'---',
		'title: "ADR template"',
		'description: "The template every architecture decision record starts from."',
		`editUrl: ${JSON.stringify(`${repo}/edit/main/${path}`)}`,
		'sidebar:',
		`  label: ${JSON.stringify(page.label)}`,
		'---',
		'',
		`Copy this into \`docs/adr/NNNN-short-title.md\` and fill it in; the [playbook](${prefix}/contributing/playbooks/write-an-adr/) has the steps.`,
		'',
		'````markdown',
		source.trimEnd(),
		'````',
		'',
	].join('\n');
}

// transform rewrites links and diagrams, leaving code blocks alone.
function transform(path, lines) {
	const result = [];
	let fence = null;
	let diagram = null;
	for (const line of lines) {
		const open = line.match(/^\s*(```+|~~~+)\s*([\w-]*)/);
		if (fence === null && open) {
			fence = open[1];
			if (open[2] === 'mermaid') diagram = [];
			else result.push(line);
			continue;
		}
		if (fence !== null) {
			if (line.trim().startsWith(fence) && line.trim().replace(/[`~]/g, '') === '') {
				fence = null;
				if (diagram) {
					result.push(`<pre class="mermaid">${escapeHTML(diagram.join('\n'))}</pre>`);
					diagram = null;
				} else result.push(line);
			} else if (diagram) diagram.push(line);
			else result.push(line);
			continue;
		}
		result.push(
			line
				.replace(/\]\(([^)\s]+)((?:\s+"[^"]*")?)\)/g, (_, target, t) => `](${link(path, target)}${t})`)
				.replace(/^(\s*\[[^\]]+\]:\s*)(\S+)/, (_, lead, target) => `${lead}${link(path, target)}`),
		);
	}
	return result;
}

// link is where a link in path to target goes on the site.
function link(path, target) {
	if (/^(?:[a-z][a-z0-9+.-]*:|#|\/)/i.test(target)) return target;
	const [file, anchor = ''] = target.split('#');
	const resolved = posix.normalize(posix.join(posix.dirname(path), decodeURI(file))).replace(/\/$/, '');
	const hash = anchor ? `#${anchor}` : '';
	const page = pages.get(resolved);
	if (page) return `${prefix}/${page.slug}/${hash}`;
	if (dirs.has(resolved)) return `${prefix}/${dirs.get(resolved)}/${hash}`;
	const abs = join(root, resolved);
	if (resolved.startsWith('..') || !existsSync(abs)) {
		broken.push(`${path}: ${target}`);
		return target;
	}
	return `${repo}/${statSync(abs).isDirectory() ? 'tree' : 'blob'}/main/${relative(root, abs)}${hash}`;
}

// describe is the first paragraph, as plain text, for search results and link previews.
function describe(lines) {
	for (const line of lines) {
		const l = line.trim();
		if (!l || /^(#|\||>|-|\*|\d+\.|```|~~~|<)/.test(l)) continue;
		const text = l
			.replace(/!?\[([^\]]*)\]\([^)]*\)/g, '$1')
			.replace(/[*_`]/g, '')
			.trim();
		return text.length > 200 ? `${text.slice(0, 197).replace(/\s+\S*$/, '')}…` : text;
	}
	return '';
}

// shortADRNames reads each ADR's short name from the ADR index ("| [0017](…) | **Name:** …").
function shortADRNames() {
	const names = new Map();
	for (const line of readFileSync(join(root, 'docs/adr/README.md'), 'utf8').split('\n')) {
		const m = line.match(/^\|\s*\[(\d{4})\]\([^)]*\)\s*\|\s*\*\*([^*]+?):?\*\*/);
		if (m) names.set(m[1], `${m[1]} · ${m[2].replace(/:$/, '')}`);
	}
	return names;
}

function escapeHTML(s) {
	return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}
