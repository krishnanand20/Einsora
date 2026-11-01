// Where the site is published, shared by the Astro config and the docs sync script.
//
// Cloudflare Pages builds and serves the site at https://einsora.com, from this repository: every
// push to main publishes it, and pull requests get preview URLs. SITE_URL and BASE_PATH override
// these, for example to publish under a sub-path somewhere else.
export const repo = 'https://github.com/krishnanand20/Einsora';
export const site = process.env.SITE_URL || 'https://einsora.com';
export const base = process.env.BASE_PATH || '/';
