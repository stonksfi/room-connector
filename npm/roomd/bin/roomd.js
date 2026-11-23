#!/usr/bin/env node
// Runs the roomd binary from the platform package npm installed with this one
// (@stonksfi/roomd-<os>-<arch>). Node is only on the path of commands typed in
// a terminal: `roomd install` copies the binary to ~/.room/bin, and launchd and
// the Claude Code hooks run that copy directly.
'use strict';

const { spawn } = require('node:child_process');

const platform = `${process.platform}-${process.arch}`;

let bin;
try {
  bin = require.resolve(`@stonksfi/roomd-${platform}/bin/roomd`);
} catch {
  console.error(
    `roomd: no build for ${platform}. roomd runs on macOS (arm64 and x64); ` +
      'on a Mac, reinstall @stonksfi/roomd without --omit=optional.',
  );
  process.exit(1);
}

const child = spawn(bin, process.argv.slice(2), { stdio: 'inherit' });
// Ctrl-C reaches roomd from the terminal too; forwarding covers signals sent
// to this process alone.
const signals = ['SIGINT', 'SIGTERM', 'SIGHUP'];
for (const sig of signals) process.on(sig, () => child.kill(sig));

child.on('error', (err) => {
  console.error(`roomd: ${err.message}`);
  process.exit(1);
});
child.on('exit', (code, signal) => {
  if (signal) {
    // Die of the same signal, so the shell sees what roomd saw.
    process.removeAllListeners(signal);
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code);
});
