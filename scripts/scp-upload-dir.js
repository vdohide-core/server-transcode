/**
 * SCP Upload Directory Script
 * Uploads an entire local directory to remote via SCP
 * 
 * Usage: node scp-upload-dir.js <json-config>
 * 
 * JSON config (base64 encoded):
 * {
 *   "localDir": "/path/to/sprite/",
 *   "host": "192.168.1.1",
 *   "port": 22,
 *   "username": "user",
 *   "password": "pass",
 *   "remotePath": "/home/files/{fileId}/sprite"
 * }
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

  const { localDir, host, port = 22, username, password, remotePath } = config;

  if (!localDir || !host || !username || !remotePath) {
    output('error', { message: 'Missing required fields: localDir, host, username, remotePath' });
    process.exit(1);
  }

  if (!fs.existsSync(localDir) || !fs.statSync(localDir).isDirectory()) {
    output('error', { message: `Directory not found: ${localDir}` });
    process.exit(1);
  }

  // Count files and total size
  const files = fs.readdirSync(localDir).filter(f => fs.statSync(path.join(localDir, f)).isFile());
  const totalSize = files.reduce((sum, f) => sum + fs.statSync(path.join(localDir, f)).size, 0);

  output('start', { fileCount: files.length, totalSize, host, remotePath });

  let client;
  try {
    const authConfig = { host, port, username, hostVerifier: () => true };
    if (password) authConfig.password = password;

    client = await Client(authConfig);

    // Ensure remote directory exists
    const dirExists = await client.exists(remotePath);
    if (!dirExists) {
      await client.mkdir(remotePath, { recursive: true });
    }

    // Upload entire directory
    await client.uploadDir(localDir, remotePath);

    await client.close();

    output('success', {
      remotePath,
      fileCount: files.length,
      totalSize
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
