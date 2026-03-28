#!/usr/bin/env node
// Copies vendored front-end dist files from node_modules into the Go embed tree.
// Run after `npm install` to update internal/web/static/vendor/ from package.json versions.

const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');
const dest = path.join(root, 'internal', 'web', 'static', 'vendor');

const files = [
  ['node_modules/bootstrap/dist/css/bootstrap.min.css',    'bootstrap.min.css'],
  ['node_modules/bootstrap/dist/js/bootstrap.bundle.min.js', 'bootstrap.bundle.min.js'],
  ['node_modules/htmx.org/dist/htmx.min.js',              'htmx.min.js'],
];

fs.mkdirSync(dest, { recursive: true });

for (const [src, name] of files) {
  const from = path.join(root, src);
  const to   = path.join(dest, name);
  fs.copyFileSync(from, to);
  const size = fs.statSync(to).size;
  console.log(`  ${name}: ${size} bytes`);
}
