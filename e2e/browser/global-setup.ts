import { spawn, type ChildProcess } from 'node:child_process'
import { writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'
import { type FullConfig } from '@playwright/test'

const __dirname = dirname(fileURLToPath(import.meta.url))

// global-setup boots the clawvisor-server subprocess (serve/main.go) and writes
// its BASE_URL to .env-browser for the config + specs to read. It returns a
// teardown function that stops the server.
//
// Authentication is per-spec (helpers.login) rather than a shared storageState:
// the server's refresh token is single-use/rotating (ConsumeSession), so a
// shared cookie jar would only authenticate the first page load.

async function globalSetup(_config: FullConfig): Promise<() => Promise<void>> {
  const { child, url } = await startServer()
  process.env.BASE_URL = url
  writeFileSync(resolve(__dirname, '.env-browser'), `BASE_URL=${url}\n`)
  return async () => {
    await stopServer(child)
  }
}

// startServer spawns `go run ./serve -port 0` and resolves once it prints its
// readiness JSON line. Inherits serve/main.go's port-collision retry.
async function startServer(): Promise<{ child: ChildProcess; url: string }> {
  const bin = process.env.CLAWVISOR_BIN
  const child = spawn('go', ['run', './serve', '-port', '0'], {
    cwd: __dirname,
    env: { ...process.env, ...(bin ? { CLAWVISOR_BIN: bin } : {}) },
    detached: true,
    stdio: ['ignore', 'pipe', 'inherit'],
  })

  return await new Promise((resolvePromise, rejectPromise) => {
    let buf = ''
    let settled = false
    const timer = setTimeout(() => {
      if (settled) return
      settled = true
      child.kill('SIGTERM')
      rejectPromise(new Error('serve did not report readiness within 60s'))
    }, 60_000)

    child.stdout!.on('data', (chunk: Buffer) => {
      buf += chunk.toString()
      const line = buf.split('\n').find((l) => l.trim().startsWith('{'))
      if (!line || settled) return
      try {
        const info = JSON.parse(line) as { url: string; pid: number }
        settled = true
        clearTimeout(timer)
        resolvePromise({ child, url: info.url })
      } catch {
        // Partial line — keep buffering.
      }
    })
    child.on('exit', (code) => {
      if (settled) return
      settled = true
      clearTimeout(timer)
      rejectPromise(new Error(`serve exited before readiness (code ${code})`))
    })
  })
}

async function stopServer(child: ChildProcess): Promise<void> {
  if (child.pid === undefined || child.exitCode !== null) return
  try {
    // Negative pid targets the detached process group (go run + server child).
    process.kill(-child.pid, 'SIGTERM')
  } catch {
    try {
      child.kill('SIGTERM')
    } catch {
      // Already gone.
    }
  }
  await new Promise<void>((r) => {
    const t = setTimeout(r, 5_000)
    child.on('exit', () => {
      clearTimeout(t)
      r()
    })
  })
}

export default globalSetup
