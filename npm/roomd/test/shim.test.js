'use strict';

// The shim, run beside a node_modules holding a fake platform package whose
// "roomd" is a shell script.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const shim = path.join(__dirname, '..', 'bin', 'roomd.js');

function install({ withBinary }) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'roomd-shim-'));
  fs.copyFileSync(shim, path.join(root, 'roomd.js'));
  if (withBinary) {
    const bin = path.join(root, 'node_modules', '@stonksfi', `roomd-${process.platform}-${process.arch}`, 'bin');
    fs.mkdirSync(bin, { recursive: true });
    fs.writeFileSync(path.join(bin, 'roomd'), '#!/bin/sh\necho "args: $*"\nexit 3\n', { mode: 0o755 });
  }
  return path.join(root, 'roomd.js');
}

test('runs the platform binary with the arguments and passes its exit code back', () => {
  const r = spawnSync(process.execPath, [install({ withBinary: true }), 'login', '--server', 'https://x'], {
    encoding: 'utf8',
  });
  assert.strictEqual(r.stdout, 'args: login --server https://x\n');
  assert.strictEqual(r.status, 3);
});

test('says which platform has no build when the platform package is missing', () => {
  const r = spawnSync(process.execPath, [install({ withBinary: false }), 'status'], { encoding: 'utf8' });
  assert.strictEqual(r.status, 1);
  assert.match(r.stderr, new RegExp(`no build for ${process.platform}-${process.arch}`));
});
