#!/usr/bin/env node
// Copies vendored front-end dist files from node_modules into the Go embed tree
// and minifies project CSS/JS. Run after `npm install` to update
// internal/web/static/ from package.json versions and web/ sources.

const fs = require('fs');
const path = require('path');
const esbuild = require('esbuild');

const root = path.join(__dirname, '..');
const dest = path.join(root, 'internal', 'web', 'static');
const vendorDest = path.join(dest, 'vendor');

// ── Vendor libs (already minified) ─────────────────────────────────────────
const vendorFiles = [
  ['node_modules/bootstrap/dist/css/bootstrap.min.css',      'bootstrap.min.css'],
  ['node_modules/bootstrap/dist/js/bootstrap.bundle.min.js', 'bootstrap.bundle.min.js'],
  ['node_modules/htmx.org/dist/htmx.min.js',                'htmx.min.js'],
];

fs.mkdirSync(vendorDest, { recursive: true });

for (const [src, name] of vendorFiles) {
  const from = path.join(root, src);
  const to   = path.join(vendorDest, name);
  fs.copyFileSync(from, to);
  const size = fs.statSync(to).size;
  console.log(`  vendor/${name}: ${size} bytes`);
}

// ── Project assets (minified from web/) ────────────────────────────────────
const projectFiles = [
  { src: 'web/app.js',    out: 'app.js',    loader: 'js'  },
  { src: 'web/style.css', out: 'style.css', loader: 'css' },
];

for (const { src, out, loader } of projectFiles) {
  const from = path.join(root, src);
  const result = esbuild.buildSync({
    entryPoints: [from],
    bundle: false,
    minify: true,
    outfile: path.join(dest, out),
    loader: { [`.${loader}`]: loader },
    logLevel: 'warning',
  });
  const size = fs.statSync(path.join(dest, out)).size;
  console.log(`  ${out}: ${size} bytes (minified)`);
}
