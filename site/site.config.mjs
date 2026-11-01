// Where the site is published, shared by the Astro config and the docs sync script.
//
// GitHub Pages serves the repository's site at https://krishnanand20.github.io/Einsora/. Once the
// project's domain is set up, build with SITE_URL=https://einsora.dev and BASE_PATH=/ (the deploy
// workflow reads both from repository variables).
export const repo = 'https://github.com/krishnanand20/Einsora';
export const site = process.env.SITE_URL || 'https://krishnanand20.github.io';
export const base = process.env.BASE_PATH || '/Einsora';
