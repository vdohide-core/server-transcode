/**
 * SCP Download Script
 * Called by Go server-transcode to download files from remote storage via SCP
 * 
 * Usage: node scp-download.js <json-config>
 * 
 * JSON config (base64 encoded):
 * {
 *   "host": "192.168.1.1",
 *   "port": 22,
 *   "username": "user",
 *   "password": "pass",
 *   "remotePath": "/home/files/fileId/file_original.mp4",
 *   "localPath": "/tmp/download/file_original.mp4"
 * }
 * 
 * Output: JSON lines
 *   {"type":"start","remotePath":"/home/files/...","host":"192.168.1.1"}
 *   {"type":"progress","percent":50,"transferred":1024,"total":2048}
 *   {"type":"success","localPath":"/tmp/download/file_original.mp4","fileSize":2048}
 *   {"type":"error","message":"Connection refused"}
 */

const scp = require('node-scp');
const Client = scp.Client || scp.default || scp;
const fs = require('fs');
const path = require('path');

async function main() {
  const configBase64 = process.argv[2];
  if (!configBase64) {
    output('error', { message: 'Missing config argument' });
    process.exit(1);
  }

  let config;
  try {
    config = JSON.parse(Buffer.from(configBase64, 'base64').toString('utf-8'));
  } catch (e) {
    output('error', { message: `Invalid config: ${e.message}` });
    process.exit(1);
  }

  const { host, port = 22, username, password, remotePath, localPath } = config;

  // Validate
  if (!host || !username || !remotePath || !localPath) {
    output('error', { message: 'Missing required fields: host, username, remotePath, localPath' });
    process.exit(1);
  }

  // Ensure local directory exists
  const localDir = path.dirname(localPath);
  if (!fs.existsSync(localDir)) {
    fs.mkdirSync(localDir, { recursive: true });
  }

  output('start', { remotePath, host, localPath });

  let client;
  try {
    // Connect
    const authConfig = { host, port, username, hostVerifier: () => true };
    if (password) authConfig.password = password;

    client = await Client(authConfig);

    // Check remote file exists
    const exists = await client.exists(remotePath);
    if (!exists) {
      throw new Error(`Remote file not found: ${remotePath}`);
    }

    let lastPercent = 0;

    // Download with progress
    await client.downloadFile(remotePath, localPath, {
      step: (transferred, chunk, total) => {
        const percent = Math.round((transferred / total) * 100);
        // Report every 1%
        if (percent >= lastPercent + 1 || percent === 100) {
          lastPercent = percent;
          output('progress', { percent, transferred, total });
        }
      }
    });

    await client.close();

    const stats = fs.statSync(localPath);
    output('success', {
      localPath,
      remotePath,
      fileSize: stats.size
    });

  } catch (err) {
    if (client) {
      try { await client.close(); } catch (_) {}
    }
    output('error', { message: err.message || String(err) });
    process.exit(1);
  }
}

function output(type, data) {
  console.log(JSON.stringify({ type, ...data }));
}

main();
