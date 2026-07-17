// Internal link checker for the built site. Walks _site/**/*.html, collects
// every root-relative href, and verifies each resolves to a built file.
// Usage: node tools/check-links.mjs   (from docs-site/)
import { readdirSync, readFileSync, statSync, existsSync } from 'node:fs';
import { join } from 'node:path';

const ROOT = new URL('../_site', import.meta.url).pathname;

function* htmlFiles(dir) {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) yield* htmlFiles(p);
    else if (name.endsWith('.html')) yield p;
  }
}

function resolves(href) {
  const path = href.split('#')[0].split('?')[0];
  if (path === '' || path === '/') return true;
  const target = join(ROOT, path);
  return (
    existsSync(target) ||
    existsSync(join(target, 'index.html')) ||
    existsSync(target + '.html')
  );
}

let failures = 0;
for (const file of htmlFiles(ROOT)) {
  const html = readFileSync(file, 'utf8');
  const hrefs = [...html.matchAll(/href="(\/[^"]*)"/g)].map(m => m[1]);
  for (const href of new Set(hrefs)) {
    if (!resolves(href)) {
      console.error(`BROKEN ${href}  (in ${file.slice(ROOT.length)})`);
      failures++;
    }
  }
}
if (failures) {
  console.error(`${failures} broken internal link(s)`);
  process.exit(1);
}
console.log('all internal links resolve');
