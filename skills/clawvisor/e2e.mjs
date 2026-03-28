#!/usr/bin/env node
// Clawvisor E2E encryption helper — zero external dependencies.
// Usage: node e2e.mjs --url <daemon_url> --token <agent_token> --body '<json>'

import {
  generateKeyPairSync, diffieHellman, createPublicKey,
  createCipheriv, createDecipheriv, randomBytes, hkdfSync
} from 'node:crypto';
import { request as httpsRequest } from 'node:https';
import { request as httpRequest } from 'node:http';
import { readFileSync, writeFileSync, unlinkSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

// X25519 SPKI DER prefix (RFC 8410). Prepend to a raw 32-byte public key
// to produce a valid SPKI structure that Node's createPublicKey accepts.
const X25519_SPKI_PREFIX = Buffer.from('302a300506032b656e032100', 'hex');

/**
 * Fetch the daemon's public key, caching in /tmp.
 * Pass bypassCache=true to ignore the cache and fetch fresh (used on
 * retry after decryption failure, which signals a stale cached key).
 */
async function fetchDaemonKey(baseURL, bypassCache = false) {
  const cacheDir = tmpdir();
  const keyURL = baseURL.replace(/\/+$/, '') + '/.well-known/clawvisor-keys';
  const parsed = new URL(keyURL);
  const cachePath = join(cacheDir, `cvis-pubkey-${parsed.hostname}.json`);

  if (!bypassCache && existsSync(cachePath)) {
    try {
      const cached = JSON.parse(readFileSync(cachePath, 'utf8'));
      if (cached.x25519) return cached;
    } catch { /* ignore corrupt cache */ }
  }

  const data = await httpGet(keyURL);
  const keys = JSON.parse(data);
  writeFileSync(cachePath, JSON.stringify(keys), { mode: 0o600 });
  return keys;
}

/**
 * Evict the cached daemon key for a given base URL.
 */
function evictKeyCache(baseURL) {
  try {
    const parsed = new URL(baseURL);
    const cachePath = join(tmpdir(), `cvis-pubkey-${parsed.hostname}.json`);
    if (existsSync(cachePath)) unlinkSync(cachePath);
  } catch { /* best effort */ }
}

/**
 * Simple HTTP(S) GET returning body as string.
 */
function httpGet(url) {
  return new Promise((resolve, reject) => {
    const mod = url.startsWith('https') ? httpsRequest : httpRequest;
    const req = mod(url, (res) => {
      let body = '';
      res.on('data', (chunk) => body += chunk);
      res.on('end', () => {
        if (res.statusCode >= 400) reject(new Error(`HTTP ${res.statusCode}: ${body}`));
        else resolve(body);
      });
    });
    req.on('error', reject);
    req.end();
  });
}

/**
 * HTTP(S) request with headers and body, returning { status, headers, body }.
 */
function httpReq(url, method, headers, body) {
  return new Promise((resolve, reject) => {
    const parsed = new URL(url);
    const mod = parsed.protocol === 'https:' ? httpsRequest : httpRequest;
    const opts = {
      hostname: parsed.hostname,
      port: parsed.port || (parsed.protocol === 'https:' ? 443 : 80),
      path: parsed.pathname + parsed.search,
      method,
      headers,
    };
    const req = mod(opts, (res) => {
      let data = '';
      res.on('data', (chunk) => data += chunk);
      res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body: data }));
    });
    req.on('error', reject);
    if (body) req.write(body);
    req.end();
  });
}

/**
 * Decrypt a response body using the shared secret.
 */
function decrypt(ciphertextB64, shared) {
  const data = Buffer.from(ciphertextB64, 'base64');
  const nonce = data.subarray(0, 12);
  const tag = data.subarray(-16);
  const enc = data.subarray(12, -16);

  const decipher = createDecipheriv('aes-256-gcm', shared, nonce);
  decipher.setAuthTag(tag);
  return Buffer.concat([decipher.update(enc), decipher.final()]).toString();
}

/**
 * Import a raw 32-byte X25519 public key into a Node KeyObject.
 */
function importX25519Pub(rawBytes) {
  return createPublicKey({
    key: Buffer.concat([X25519_SPKI_PREFIX, rawBytes]),
    format: 'der',
    type: 'spki',
  });
}

/**
 * Create an E2E client for making encrypted requests to the daemon.
 *
 * On decryption failure (stale cached daemon key after key regeneration),
 * the client evicts the cache, fetches a fresh key, and retries once.
 */
export async function createClient(daemonURL, agentToken) {
  let daemonKeyObj = await loadDaemonKey(daemonURL, false);

  async function sendEncrypted(method, endpoint, body) {
    const { publicKey: ephPub, privateKey: ephPriv } = generateKeyPairSync('x25519');
    const rawShared = diffieHellman({ privateKey: ephPriv, publicKey: daemonKeyObj });
    const shared = Buffer.from(hkdfSync('sha256', rawShared, Buffer.alloc(0), 'clawvisor-e2e-v1', 32));

    const ephPubRaw = ephPub.export({ type: 'spki', format: 'der' }).subarray(-32);

    const headers = {
      'X-Clawvisor-E2E': 'aes-256-gcm',
      'X-Clawvisor-Ephemeral-Key': ephPubRaw.toString('base64'),
    };
    if (agentToken) headers['Authorization'] = `Bearer ${agentToken}`;

    let reqBody = null;
    if (body != null) {
      // Encrypt request body: nonce(12) || ciphertext || tag(16)
      const nonce = randomBytes(12);
      const cipher = createCipheriv('aes-256-gcm', shared, nonce);
      const encrypted = Buffer.concat([cipher.update(Buffer.from(JSON.stringify(body))), cipher.final()]);
      const tag = cipher.getAuthTag();
      const ciphertext = Buffer.concat([nonce, encrypted, tag]);
      headers['Content-Type'] = 'application/octet-stream';
      reqBody = ciphertext.toString('base64');
    }

    const resp = await httpReq(`${daemonURL}${endpoint}`, method, headers, reqBody);

    const raw = resp.headers['x-clawvisor-e2e']
      ? decrypt(resp.body, shared)
      : resp.body;

    const ct = resp.headers['content-type'] || '';
    if (ct.includes('json')) return JSON.parse(raw);
    return raw;
  }

  async function requestWithRetry(method, endpoint, body) {
    try {
      const result = await sendEncrypted(method, endpoint, body);
      // Check for E2E_ERROR in the response (decryption failed server-side
      // because we used a stale key).
      if (result?.code === 'E2E_ERROR') throw new Error('E2E error');
      return result;
    } catch (err) {
      // Evict stale key, fetch fresh, and retry once.
      evictKeyCache(daemonURL);
      daemonKeyObj = await loadDaemonKey(daemonURL, true);
      return sendEncrypted(method, endpoint, body);
    }
  }

  return {
    /** Generic encrypted request. */
    async request(method, endpoint, body) {
      return requestWithRetry(method, endpoint, body);
    },
    /** Shorthand for POST /api/gateway/request. */
    async gatewayRequest(body) {
      return requestWithRetry('POST', '/api/gateway/request', body);
    },
  };
}

/**
 * Load and import the daemon's X25519 public key.
 */
async function loadDaemonKey(daemonURL, bypassCache) {
  const keys = await fetchDaemonKey(daemonURL, bypassCache);
  const daemonPubRaw = Buffer.from(keys.x25519, 'base64');
  return importX25519Pub(daemonPubRaw);
}

// CLI entry point
if (process.argv[1]?.endsWith('e2e.mjs')) {
  const args = process.argv.slice(2);
  let url, token, body, endpoint = '/api/gateway/request', method = 'POST';
  for (let i = 0; i < args.length; i++) {
    if (args[i] === '--url') url = args[++i];
    else if (args[i] === '--token') token = args[++i];
    else if (args[i] === '--body') body = args[++i];
    else if (args[i] === '--endpoint') endpoint = args[++i];
    else if (args[i] === '--method') method = args[++i];
  }
  if (!url) {
    console.error('Usage: node e2e.mjs --url <daemon_url> [--token <agent_token>] [--endpoint /api/...] [--method POST] [--body \'<json>\']');
    process.exit(1);
  }
  const client = await createClient(url, token);
  const result = await client.request(method, endpoint, body ? JSON.parse(body) : null);
  console.log(JSON.stringify(result, null, 2));
}
