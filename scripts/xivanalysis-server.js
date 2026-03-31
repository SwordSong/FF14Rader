#!/usr/bin/env node
// HTTP analysis server for xivanalysis. Built-in http only; no external deps.
// POST /analyze with payload {code, report, fightId, events} or {requests:[...]}

const http = require('http')
const fs = require('fs')
const path = require('path')
const Module = require('module')

const repoRoot = path.resolve(__dirname, '..')
const xivaRoot = path.resolve(repoRoot, 'external', 'xivanalysis')
const xivaSrc = path.resolve(xivaRoot, 'src')

const PORT = Number(process.env.PORT || process.env.port || 22026) || 3000
const HOST = process.env.HOST || '0.0.0.0'
const MAX_BODY_BYTES = Number(process.env.MAX_BODY_BYTES || 20 * 1024 * 1024) || 20 * 1024 * 1024
const MAX_CONCURRENCY = Number(process.env.MAX_CONCURRENCY || 2) || 2

let inFlight = 0
let cachedModules = null

bootstrap()
startServer()

function bootstrap() {
  if (!process.env.LINGUI_CONFIG) {
    process.env.LINGUI_CONFIG = path.join(xivaRoot, 'lingui.config.ts')
  }
  if (!process.env.NODE_ENV) {
    process.env.NODE_ENV = 'test'
  }

  require(path.join(xivaRoot, 'node_modules', '@babel', 'register'))({
    cwd: xivaRoot,
    rootMode: 'upward',
    extensions: ['.ts', '.tsx', '.js', '.jsx'],
    cache: false,
  })

  registerAssetStubs()
  registerGlobals()
  loadModulesOnce()
  patchMissingDependencies()
}

function loadModulesOnce() {
  if (cachedModules) {
    return cachedModules
  }
  process.env.NODE_PATH = process.env.NODE_PATH
    ? `${process.env.NODE_PATH}${path.delimiter}${xivaSrc}`
    : xivaSrc
  Module._initPaths()
  registerAliases(xivaSrc)

  const encounters = require(path.join(xivaSrc, 'data/ENCOUNTERS'))
  const editions = require(path.join(xivaSrc, 'data/EDITIONS'))
  const available = require(path.join(xivaSrc, 'parser/AVAILABLE_MODULES'))
  const parser = require(path.join(xivaSrc, 'parser/core/Parser'))
  const adapter = require(path.join(xivaSrc, 'reportSources/legacyFflogs/eventAdapter/adapter'))
  const report = require(path.join(xivaSrc, 'report'))
    
  cachedModules = {
    getEncounterKey: encounters.getEncounterKey,
    GameEdition: editions.GameEdition,
    AVAILABLE_MODULES: available.AVAILABLE_MODULES,
    Parser: parser.Parser,
    adaptEvents: adapter.adaptEvents,
    Team: report.Team,
  }
  return cachedModules
}

function startServer() {
  const server = http.createServer((req, res) => {
    if (req.method === 'GET' && req.url === '/health') {
      res.writeHead(200, {'Content-Type': 'application/json'})
      res.end(JSON.stringify({status: 'ok'}))
      return
    }
    if (req.method !== 'POST' || req.url !== '/analyze') {
      res.writeHead(404, {'Content-Type': 'application/json'})
      res.end(JSON.stringify({error: 'not found'}))
      return
    }

    if (inFlight >= MAX_CONCURRENCY) {
      res.writeHead(429, {'Content-Type': 'application/json'})
      res.end(JSON.stringify({error: 'server busy'}))
      return
    }

    let body = Buffer.alloc(0)
    req.on('data', chunk => {
      body = Buffer.concat([body, chunk])
      if (body.length > MAX_BODY_BYTES) {
        res.writeHead(413, {'Content-Type': 'application/json'})
        res.end(JSON.stringify({error: 'payload too large'}))
        req.destroy()
      }
    })

    req.on('end', async () => {
      const parsed = parseJSONSafe(body)
      if (!parsed.ok) {
        res.writeHead(400, {'Content-Type': 'application/json'})
        res.end(JSON.stringify({error: `invalid JSON: ${parsed.error}`}))
        return
      }

      inFlight += 1
      try {
        const payload = parsed.value || {}
        const results = await handleAnalyze(payload)
        res.writeHead(200, {'Content-Type': 'application/json'})
        res.end(JSON.stringify({results}))
      } catch (err) {
    const payload = formatError(err)
    console.error('[xiva] analyze error:', payload.error)
    if (payload.stack) {
      console.error(payload.stack)
    }
    res.writeHead(400, {'Content-Type': 'application/json'})
    res.end(JSON.stringify(payload))
      } finally {
        inFlight -= 1
      }
    })
    req.on('error', err => {
      res.writeHead(500, {'Content-Type': 'application/json'})
      res.end(JSON.stringify({error: `request error: ${err.message}`}))
    })
  })

  server.listen(PORT, HOST, () => {
    console.log(`xivanalysis server listening on http://${HOST}:${PORT}`)
  })
}

function formatError(err) {
  const message = err && err.message ? err.message : String(err)
  const includeStack = process.env.DEBUG_ERRORS === '1'
  return includeStack
    ? {error: message, stack: err && err.stack ? String(err.stack) : undefined}
    : {error: message}
}

async function handleAnalyze(payload) {
  const requests = Array.isArray(payload.requests) ? payload.requests : [payload]
  if (requests.length === 0) {
    throw new Error('requests must be a non-empty array')
  }

  const results = []
  for (let i = 0; i < requests.length; i++) {
    const req = requests[i]
    const result = await analyzeRequest(req, i)
    results.push(result)
  }
  return results
}

async function analyzeRequest(request, index) {
  const mods = loadModulesOnce()
  const normalized = normalizeRequest(request)
  if (normalized.error) {
    return {
      index,
      status: 'error',
      error: normalized.error,
    }
  }

  const {code, report, fight, fightId, events} = normalized
  const actorIdsInFight = collectActorIds(events)
  const pull = buildPull(mods, report, fight, actorIdsInFight)
  const xvReport = buildReport(mods, code, report, [pull])
  const firstEvent = report.startTime + fight.startTime
  const adapted = mods.adaptEvents(xvReport, pull, events, firstEvent)

  const resultsByActor = []
  for (const actor of pull.actors) {
    if (actor.team !== mods.Team.FRIEND || !actor.playerControlled) {
      continue
    }

    const meta = buildMeta(mods, pull.encounter.key, actor.job)
    const parser = new mods.Parser({
      meta,
      report: xvReport,
      pull,
      actor,
    })

    preseedCoreModules(parser)
    await parser.configure()
    parser.parseEvents({events: adapted})
    const parsedResults = parser.generateResults()
    resultsByActor.push({
      actorId: actor.id,
      name: actor.name,
      job: actor.job,
      modules: serializeResults(parsedResults),
    })
  }

  return {
    status: 'ok',
    reportCode: code,
    fightId,
    fightName: fight.name,
    startTime: report.startTime + fight.startTime,
    endTime: report.startTime + fight.endTime,
    actors: resultsByActor,
  }
}

function normalizeRequest(request) {
  if (!request || typeof request !== 'object') {
    return {error: 'request must be an object'}
  }
  const code = request.code || request.reportCode || request.report_code || 'unknown'
  const report = request.report || request.metadata || request.reportData
  const events = normalizeEvents(request.events || request.eventData || request.event_data)
  const fightId = Number(request.fightId || request.fight_id || request.fight || 0)
  const fight = request.fight

  if (!report || typeof report !== 'object') {
    return {error: 'missing report metadata'}
  }
  const normalizedReport = normalizeReport(report, request.actors || request.friendlies || request.masterData)
  if (normalizedReport.error) {
    return {error: normalizedReport.error}
  }
  if (!events || !Array.isArray(events)) {
    return {error: 'missing events array'}
  }

  let selectedFight = fight
  if (!selectedFight && Array.isArray(normalizedReport.fights)) {
    selectedFight = normalizedReport.fights.find(f => Number(f.id) === fightId)
  }
  if (!selectedFight) {
    return {error: 'fight not found in report; provide fight or fightId'}
  }

  return {
    code,
    report: normalizedReport,
    fight: selectedFight,
    fightId: Number(selectedFight.id),
    events,
  }
}

function normalizeReport(report, actorsFallback) {
  const missing = []
  if (report.startTime == null) missing.push('startTime')
  if (report.endTime == null) missing.push('endTime')
  if (!report.title) missing.push('title')
  if (!Array.isArray(report.fights)) missing.push('fights')
  if (missing.length > 0) {
    return {error: `report missing fields: ${missing.join(', ')}`}
  }

  let masterData = report.masterData
  if (!masterData || !Array.isArray(masterData.actors)) {
    const actors = normalizeActors(actorsFallback || report.actors || report.friendlies)
    if (!actors) {
      return {error: 'report.masterData.actors missing; supply actors'}
    }
    masterData = {actors}
  }

  return {
    ...report,
    masterData,
  }
}

function normalizeActors(input) {
  if (!input) return null
  if (Array.isArray(input)) return input
  if (input && Array.isArray(input.actors)) return input.actors
  return null
}

function normalizeEvents(raw) {
  if (!raw) return null
  if (Array.isArray(raw)) return raw
  if (raw && Array.isArray(raw.events)) return raw.events
  if (raw && raw.events && Array.isArray(raw.events.events)) return raw.events.events
  return null
}

function parseJSONSafe(buf) {
  try {
    return {ok: true, value: JSON.parse(buf)}
  } catch (err) {
    return {ok: false, error: err.message}
  }
}

function buildReport(mods, code, data, pulls) {
  return {
    timestamp: data.startTime,
    edition: mods.GameEdition.GLOBAL,
    name: data.title,
    pulls,
    meta: {
      source: 'legacyFflogs',
      code,
      end: data.endTime,
      fights: data.fights,
      friendlies: [],
      enemies: [],
    },
  }
}

function buildPull(mods, report, fight, actorIdsInFight) {
  const encounterKey = fight.encounterID != null
    ? mods.getEncounterKey('legacyFflogs', String(fight.encounterID))
    : undefined

  const actors = report.masterData.actors
    .filter(a => !actorIdsInFight || actorIdsInFight.has(a.id))
    .map(a => buildActor(mods, a))

  return {
    id: fight.id.toString(),
    timestamp: report.startTime + fight.startTime,
    duration: fight.endTime - fight.startTime,
    progress: fight.kill ? 100 : fight.fightPercentage != null ? 100 - fight.fightPercentage / 100 : undefined,
    encounter: {
      key: encounterKey,
      name: fight.name,
      duty: {
        id: fight.gameZone ? fight.gameZone.id : 0,
        name: fight.gameZone ? fight.gameZone.name : 'Unknown',
      },
    },
    actors,
  }
}

function collectActorIds(events) {
  const ids = new Set()
  for (const event of events) {
    const sourceId = event.sourceID ?? event.sourceId
    const targetId = event.targetID ?? event.targetId
    if (sourceId != null) ids.add(sourceId)
    if (targetId != null) ids.add(targetId)
    if (Array.isArray(event.targets)) {
      for (const target of event.targets) {
        const tid = target.targetID ?? target.targetId
        if (tid != null) ids.add(tid)
      }
    }
  }
  return ids
}

function buildActor(mods, actor) {
  const isPlayer = actor.type === 'Player'
  return {
    kind: actor.gameID != null ? String(actor.gameID) : String(actor.id),
    name: actor.name,
    team: isPlayer ? mods.Team.FRIEND : mods.Team.FOE,
    playerControlled: isPlayer,
    job: normalizeJobKey(actor.subType),
    id: actor.id,
  }
}

function normalizeJobKey(subType) {
  if (!subType || typeof subType !== 'string') {
    return 'UNKNOWN'
  }
  return subType.trim().toUpperCase().replace(/[-\s]+/g, '_')
}

function buildMeta(mods, encounterKey, job) {
  let meta = mods.AVAILABLE_MODULES.CORE
  if (encounterKey && mods.AVAILABLE_MODULES.BOSSES[encounterKey]) {
    meta = meta.merge(mods.AVAILABLE_MODULES.BOSSES[encounterKey])
  }
  if (mods.AVAILABLE_MODULES.JOBS[job]) {
    meta = meta.merge(mods.AVAILABLE_MODULES.JOBS[job])
  }
  return meta
}

function serializeResults(results) {
  const React = require(path.join(xivaRoot, 'node_modules', 'react'))
  const {renderToStaticMarkup} = require(path.join(xivaRoot, 'node_modules', 'react-dom', 'server'))
  return results.map(result => {
    let html = ''
    try {
      html = renderToStaticMarkup(result.markup)
    } catch (_err) {
      html = '[render-error]'
    }
    const name = result.name && result.name.id ? result.name.id : result.handle
    return {
      handle: result.handle,
      name,
      mode: result.mode,
      order: result.order,
      html,
    }
  })
}

function patchMissingDependencies() {
  const statusTimelinePath = path.join(xivaSrc, 'parser/core/modules/StatusTimeline')
  let StatusTimeline
  try {
    StatusTimeline = require(statusTimelinePath).StatusTimeline
  } catch (_err) {
    return
  }

  if (!StatusTimeline || StatusTimeline.prototype._xivaDepsPatched) {
    return
  }

  const originalInit = StatusTimeline.prototype.initialise
  StatusTimeline.prototype.initialise = function () {
    if (!this.actionTimeline) {
      this.actionTimeline = this.parser.container.actionTimeline
    }
    if (!this.actors) {
      this.actors = this.parser.container.actors
    }
    if (!this.data) {
      this.data = this.parser.container.data
    }
    if (!this.data || !this.data.actions) {
      console.warn('[xiva] StatusTimeline missing data/actions; skipping initialisation')
      return
    }
    return originalInit.call(this)
  }
  StatusTimeline.prototype._xivaDepsPatched = true

  const resourceGraphsPath = path.join(xivaSrc, 'parser/core/modules/ResourceGraphs')
  let ResourceGraphs
  try {
    ResourceGraphs = require(resourceGraphsPath).ResourceGraphs
  } catch (_err) {
    return
  }

  if (!ResourceGraphs || ResourceGraphs.prototype._xivaDepsPatched) {
    return
  }

  const originalAddDataGroup = ResourceGraphs.prototype.addDataGroup
  ResourceGraphs.prototype.addDataGroup = function (...args) {
    if (!this.timeline && this.parser && this.parser.container) {
      this.timeline = this.parser.container.timeline
    }
    return originalAddDataGroup.apply(this, args)
  }
  ResourceGraphs.prototype._xivaDepsPatched = true
}

function preseedCoreModules(parser) {
  if (!parser.container) {
    parser.container = {}
  }
  try {
    if (!parser.container.timeline) {
      const Timeline = require(path.join(xivaSrc, 'parser/core/modules/Timeline')).Timeline
      parser.container.timeline = new Timeline(parser)
    }
  } catch (err) {
    console.warn(`[xiva] failed to preseed Timeline: ${err.message || err}`)
  }
  try {
    if (!parser.container.data) {
      const Data = require(path.join(xivaSrc, 'parser/core/modules/Data')).Data
      parser.container.data = new Data(parser)
    }
  } catch (err) {
    console.warn(`[xiva] failed to preseed Data: ${err.message || err}`)
  }
  try {
    if (!parser.container.suggestions) {
      const Suggestions = require(path.join(xivaSrc, 'parser/core/modules/Suggestions')).Suggestions
      parser.container.suggestions = new Suggestions(parser)
    }
  } catch (err) {
    console.warn(`[xiva] failed to preseed Suggestions: ${err.message || err}`)
  }
  try {
    if (!parser.container.resourceGraphs) {
      const ResourceGraphs = require(path.join(xivaSrc, 'parser/core/modules/ResourceGraphs')).ResourceGraphs
      parser.container.resourceGraphs = new ResourceGraphs(parser)
    }
  } catch (err) {
    console.warn(`[xiva] failed to preseed ResourceGraphs: ${err.message || err}`)
  }
}

function registerAliases(srcPath) {
  const aliasRoots = new Set([
    'parser',
    'components',
    'utilities',
    'reportSources',
    'report',
    'errors',
    'event',
    'env',
    'store',
  ])

  const originalResolve = Module._resolveFilename
  Module._resolveFilename = function (request, parent, isMain, options) {
    const root = request.split('/')[0]
    if (aliasRoots.has(root)) {
      const basePath = path.join(srcPath, request)
      const resolved = resolveWithExtensions(basePath)
      if (resolved) {
        return originalResolve.call(this, resolved, parent, isMain, options)
      }
    }
    return originalResolve.call(this, request, parent, isMain, options)
  }
}

function resolveWithExtensions(basePath) {
  const candidates = [
    basePath,
    `${basePath}.ts`,
    `${basePath}.tsx`,
    `${basePath}.js`,
    path.join(basePath, 'index.ts'),
    path.join(basePath, 'index.tsx'),
    path.join(basePath, 'index.js'),
  ]

  for (const candidate of candidates) {
    if (fs.existsSync(candidate)) {
      return candidate
    }
  }

  return undefined
}

function registerAssetStubs() {
  const emptyExport = module => {
    module.exports = ''
  }
  const emptyObject = module => {
    module.exports = {}
  }
  const assetExts = ['.jpg', '.jpeg', '.png', '.gif', '.svg', '.webp']
  for (const ext of assetExts) {
    require.extensions[ext] = emptyExport
  }
  const styleExts = ['.css', '.scss', '.sass', '.less']
  for (const ext of styleExts) {
    require.extensions[ext] = emptyObject
  }
}

function registerGlobals() {
  if (typeof global.localStorage === 'undefined') {
    global.localStorage = {
      getItem() {
        return null
      },
      setItem() {},
      removeItem() {},
      clear() {},
    }
  }
  if (typeof global.sessionStorage === 'undefined') {
    global.sessionStorage = {
      getItem() {
        return null
      },
      setItem() {},
      removeItem() {},
      clear() {},
    }
  }
  if (typeof global.window === 'undefined') {
    global.window = {
      location: {
        reload() {},
      },
      Date,
      performance: {
        now() {
          return Date.now()
        },
      },
    }
  }
  if (typeof global.WheelEvent === 'undefined') {
    global.WheelEvent = function WheelEvent() {}
    global.window.WheelEvent = global.WheelEvent
  }
  if (typeof global.requestAnimationFrame === 'undefined') {
    global.requestAnimationFrame = callback => setTimeout(() => callback(Date.now()), 0)
  }
  if (typeof global.cancelAnimationFrame === 'undefined') {
    global.cancelAnimationFrame = id => clearTimeout(id)
  }
  if (typeof global.window.requestAnimationFrame === 'undefined') {
    global.window.requestAnimationFrame = global.requestAnimationFrame
  }
  if (typeof global.window.cancelAnimationFrame === 'undefined') {
    global.window.cancelAnimationFrame = global.cancelAnimationFrame
  }
}
