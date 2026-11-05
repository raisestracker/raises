#!/usr/bin/env node
'use strict'

// npm's resolver filters new resolutions, but npm 11.19.1 trusts lockfile ages.
// Check locked registry versions before letting the real CLI install anything.
const fs = require('node:fs')
const path = require('node:path')
const { createRequire } = require('node:module')
const { spawn } = require('node:child_process')

const [tool, npmRoot, ...args] = process.argv.slice(2)
const DAY = 86400000
const installing = new Set(['install', 'ci', 'update', 'dedupe', 'exec', 'init', 'audit', 'uninstall', 'prune', 'rebuild', 'link'])

function preserveLaunchers() {
  // npm self-updates can replace its bin links. Reapply only managed launchers.
  if (path.resolve(__dirname) !== path.join(process.env.HOME, '.local/lib/npm-age-policy')) return
  const bin = path.resolve(npmRoot, '../../../bin')
  for (const name of ['npm', 'npx']) {
    const quote = value => `'${value.replaceAll("'", "'\\''")}'`
    const content = `#!/bin/sh\nexec ${quote(process.execPath)} ${quote(__filename)} ${quote(name)} ${quote(npmRoot)} "$@"\n`
    const temporary = path.join(bin, `.${name}-age-policy-${process.pid}`)
    fs.writeFileSync(temporary, content, { mode: 0o755, flag: 'wx' })
    fs.renameSync(temporary, path.join(bin, name))
  }
}

function lockedPackages(lock) {
  const packages = []
  if (lock.packages && Number(lock.lockfileVersion) >= 2) {
    for (const [location, entry] of Object.entries(lock.packages)) {
      if (!location || entry.link || !location.includes('node_modules/')) continue
      packages.push({ name: entry.name || location.split('node_modules/').pop(), ...entry })
    }
  } else if (lock.lockfileVersion === 1) {
    const walk = deps => {
      for (const [name, entry] of Object.entries(deps || {})) {
        packages.push({ name, ...entry })
        walk(entry.dependencies)
      }
    }
    walk(lock.dependencies)
  } else {
    throw new Error('Unsupported lockfile format; age cannot be verified.')
  }
  return packages
}

async function checkLock(filename, cutoff, registryFor = () => 'https://registry.npmjs.org/') {
  const entries = lockedPackages(JSON.parse(fs.readFileSync(filename, 'utf8')))
  const byName = new Map()
  for (const entry of entries) {
    if (!entry.name || !/^(@[a-z0-9._-]+\/)?[a-z0-9._-]+$/i.test(entry.name) ||
        !/^\d+\.\d+\.\d+(?:[-+].*)?$/.test(entry.version || '')) {
      throw new Error('A locked dependency has no verifiable registry name/version.')
    }
    if (new URL(registryFor(entry.name)).href !== 'https://registry.npmjs.org/') {
      throw new Error('A scoped non-public registry needs explicit approval for age verification.')
    }
    if (entry.resolved) {
      let url
      try { url = new URL(entry.resolved) } catch { /* fail closed below */ }
      if (!url || url.protocol !== 'https:' || url.hostname !== 'registry.npmjs.org' ||
          url.username || url.password || url.port || url.search || url.hash) {
        throw new Error(`${entry.name}: non-public-registry lock source needs explicit approval.`)
      }
    }
    if (!byName.has(entry.name)) byName.set(entry.name, new Set())
    byName.get(entry.name).add(entry.version)
  }
  const queue = [...byName.entries()]
  await Promise.all(Array.from({ length: Math.min(6, queue.length) }, async () => {
    while (queue.length) {
      const [name, versions] = queue.shift()
      // No npm auth headers, registry override, proxy credentials, or disk cache.
      const response = await fetch(`https://registry.npmjs.org/${encodeURIComponent(name)}`, {
        redirect: 'error', signal: AbortSignal.timeout(15000),
        headers: { accept: 'application/json' },
      })
      if (!response.ok) throw new Error(`${name}: registry metadata unavailable (${response.status}).`)
      let size = 0
      const chunks = []
      for await (const chunk of response.body) {
        size += chunk.length
        if (size > 64 * 1024 * 1024) throw new Error(`${name}: metadata exceeds size limit.`)
        chunks.push(chunk)
      }
      const metadata = JSON.parse(Buffer.concat(chunks).toString('utf8'))
      for (const version of versions) {
        const published = Date.parse(metadata.time?.[version])
        if (!metadata.versions?.[version] || !Number.isFinite(published)) {
          throw new Error(`${name}@${version}: publication time cannot be verified.`)
        }
        if (published > cutoff) {
          throw new Error(`${name}@${version}: published ${new Date(published).toISOString()}, less than seven days ago.`)
        }
      }
    }
  }))
}

async function main() {
  if (!['npm', 'npx'].includes(tool) || !path.isAbsolute(npmRoot || '')) throw new Error('Invalid launcher.')
  const req = createRequire(path.join(npmRoot, 'package.json'))
  const Config = req('@npmcli/config')
  const { definitions, flatten, nerfDarts, shorthands } = req('@npmcli/config/lib/definitions')
  const { deref } = req('./lib/utils/cmd-list.js')
  const parsedArgs = tool === 'npx' ? ['exec', ...args.map(a => a === '-p' ? '--package' : a)] : args
  const config = new Config({ npmPath: npmRoot, definitions, flatten, nerfDarts, shorthands,
    argv: [process.execPath, path.join(npmRoot, 'bin/npm-cli.js'), ...parsedArgs], warn: false })
  await config.load()
  const command = tool === 'npx' ? 'exec' : deref(config.parsedArgv.remain[0])
  const auditFix = command !== 'audit' || config.parsedArgv.remain[1] === 'fix'
  const mutating = installing.has(command) && auditFix && !config.get('help')
  const override = process.env.LABOUNTY_NPM_AGE_OVERRIDE
  if (mutating && override) {
    if (override.trim().length < 12) throw new Error('Override requires a descriptive owner-approval reference.')
    console.error('npm-age-policy: explicit owner-approved override used for this invocation.')
    const state = path.join(process.env.HOME, '.local/state/npm-age-policy')
    fs.mkdirSync(state, { recursive: true, mode: 0o700 })
    fs.appendFileSync(path.join(state, 'overrides.jsonl'), JSON.stringify({
      at: new Date().toISOString(), approval: override.slice(0, 500), tool, command,
    }) + '\n', { mode: 0o600 })
    process.env.npm_config_min_release_age = '0'
  } else if (mutating) {
    const age = Number(config.get('min-release-age'))
    const before = config.get('before')
    const cutoff = Date.now() - 7 * DAY
    if (age < 7 || !Number.isFinite(age) || (before && Date.parse(before) > cutoff) ||
        config.get('min-release-age-exclude').length) {
      throw new Error('Age policy was relaxed by configuration; explicit owner approval is required.')
    }
    if (new URL(config.get('registry')).href !== 'https://registry.npmjs.org/') {
      throw new Error('A non-public registry needs explicit approval for age verification.')
    }
    const npa = req('npm-package-arg')
    const registryFor = name => (name.startsWith('@') && config.get(`${name.split('/')[0]}:registry`)) || config.get('registry')
    const remaining = config.parsedArgv.remain.slice(1)
    const specs = ['install', 'update', 'link'].includes(command) ? remaining :
      command === 'exec' ? [...config.get('package'), ...remaining.slice(0, 1)] : []
    for (const spec of specs) {
      const parsed = npa(spec)
      if (!parsed.registry) throw new Error('Installing from a Git, file, directory or tarball spec needs explicit owner approval; registry publication age is unavailable.')
      if (new URL(registryFor(parsed.name)).href !== 'https://registry.npmjs.org/') throw new Error('A scoped non-public registry needs explicit approval for age verification.')
    }
    // Check the project selected by npm, including workspace-root lockfiles.
    // Global installs have no project lock; npm applies its native age resolver.
    if (!config.get('global')) {
      for (const name of ['npm-shrinkwrap.json', 'package-lock.json']) {
        const filename = path.join(config.localPrefix, name)
        if (fs.existsSync(filename)) {
          await checkLock(filename, Math.min(cutoff, before ? Date.parse(before) : cutoff), registryFor)
          break // shrinkwrap takes precedence, matching npm
        }
      }
    }
    process.env.npm_config_min_release_age = String(Math.max(7, age))
  }
  const child = spawn(process.execPath, [path.join(npmRoot, `bin/${tool}-cli.js`), ...args], {
    stdio: 'inherit', env: process.env,
  })
  child.on('error', () => { console.error('npm-age-policy: could not start npm.'); process.exitCode = 1 })
  child.on('exit', (code, signal) => {
    if (mutating && config.get('global')) {
      try { preserveLaunchers() } catch {
        console.error('npm-age-policy: launcher restoration failed; stop installations and repair the guard.')
        process.exitCode = 1
        return
      }
    }
    if (signal) process.kill(process.pid, signal)
    else process.exitCode = code ?? 1
  })
}

if (require.main === module) main().catch(error => {
  console.error(`npm-age-policy: BLOCKED: ${error.message}`)
  console.error('Wait for the release to age, or ask Clayton for an explicit per-invocation override. Do not disable the guard.')
  process.exitCode = 1
})
module.exports = { lockedPackages, checkLock }
