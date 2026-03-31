#!/usr/bin/env node
// Analyze a single fight using local V1 events JSON and xivanalysis modules.

const fs = require('fs')
const path = require('path')
const Module = require('module')

const repoRoot = path.resolve(__dirname, '..')
const xivaRoot = path.resolve(repoRoot, 'external', 'xivanalysis')
const xivaSrc = path.resolve(xivaRoot, 'src')

if (!process.env.LINGUI_CONFIG) {
	process.env.LINGUI_CONFIG = path.join(xivaRoot, 'lingui.config.ts')
}

require(path.join(xivaRoot, 'node_modules', '@babel', 'register'))({
	cwd: xivaRoot,
	rootMode: 'upward',
	extensions: ['.ts', '.tsx', '.js', '.jsx'],
	cache: false,
})

registerAssetStubs()
registerGlobals()
if (!process.env.NODE_ENV) {
	process.env.NODE_ENV = 'test'
}

const React = require(path.join(xivaRoot, 'node_modules', 'react'))
const {renderToStaticMarkup} = require(path.join(xivaRoot, 'node_modules', 'react-dom', 'server'))

main().catch(err => {
	console.error(err)
	process.exit(1)
})

async function main() {
	const opts = parseArgs(process.argv.slice(2))
	loadEnvFile(findEnvPath())

	const mods = loadModules()
	patchMissingDependencies()

	const clientId = process.env.FFLOGS_CLIENT_ID
	const clientSecret = process.env.FFLOGS_CLIENT_SECRET
	if (!clientId || !clientSecret) {
		throw new Error('missing FFLOGS_CLIENT_ID or FFLOGS_CLIENT_SECRET')
	}

	const inputPath = path.resolve(opts.input)
	const {code, fightId} = resolveFightRef(inputPath, opts)

	const outPath = opts.outPath
		? path.resolve(opts.outPath)
		: path.resolve(path.dirname(inputPath), `fight_${fightId}_modules.json`)

	console.log(`input: ${inputPath}`)
	console.log(`report: ${code}, fight: ${fightId}`)
	console.log('auth: requesting token')
	const token = await getAccessToken(clientId, clientSecret)
	console.log('auth: ok')

	console.log('report: fetching metadata')
	const report = await fetchReportData(token, code)
	const fight = report.fights.find(f => f.id === fightId)
	if (!fight) {
		throw new Error(`fight ${fightId} not found in report ${code}`)
	}

	console.log('events: reading local file')
	const events = loadEventsFile(inputPath)
	console.log(`events: ${events.length} rows`)

	const actorIdsInFight = collectActorIds(events)
	const pull = buildPull(mods, report, fight, actorIdsInFight)
	const xvReport = buildReport(mods, code, report, [pull])

	const firstEvent = report.startTime + fight.startTime
	console.log('events: adapting')
	const adapted = mods.adaptEvents(xvReport, pull, events, firstEvent)
	console.log(`events: adapted ${adapted.length}`)

	const resultsByActor = []
	for (const actor of pull.actors) {
		if (actor.team !== mods.Team.FRIEND || !actor.playerControlled) {
			continue
		}

		console.log(`actor: ${actor.name} (${actor.job})`)
		const meta = buildMeta(mods, pull.encounter.key, actor.job)
		const metaModules = await meta.getModules()
		console.log(`meta: modules ${metaModules.length}`)
		const parser = new mods.Parser({
			meta,
			report: xvReport,
			pull,
			actor,
		})

		preseedCoreModules(parser)

		await parser.configure()
		parser.parseEvents({events: adapted})
		const results = parser.generateResults()
		console.log(`results: ${results.length}`)
		if (results.length === 0 && parser.executionOrder) {
			console.log(`results: executionOrder ${parser.executionOrder.length}`)
		}
		resultsByActor.push({
			actorId: actor.id,
			name: actor.name,
			job: actor.job,
			modules: serializeResults(results),
		})
	}

	const output = {
		reportCode: code,
		fightId: fight.id,
		fightName: fight.name,
		startTime: report.startTime + fight.startTime,
		endTime: report.startTime + fight.endTime,
		actors: resultsByActor,
	}

	writeJson(outPath, output)
	console.log(`written: ${outPath}`)
}

function parseArgs(args) {
	let input = ''
	let outPath = ''
	let code = ''
	let fightId = 0

	for (let i = 0; i < args.length; i++) {
		const arg = args[i]
		if (arg === '--input' && args[i + 1]) {
			input = args[i + 1]
			i++
		} else if (arg === '--out' && args[i + 1]) {
			outPath = args[i + 1]
			i++
		} else if (arg === '--code' && args[i + 1]) {
			code = args[i + 1]
			i++
		} else if (arg === '--fight-id' && args[i + 1]) {
			fightId = Number(args[i + 1])
			i++
		}
	}

	if (!input) {
		console.error('Usage: node scripts/analyze-events.js --input <fight_events.json> [--out <file>] [--code <report>] [--fight-id <id>]')
		process.exit(1)
	}
	return {input, outPath, code, fightId}
}

function resolveFightRef(inputPath, opts) {
	let code = opts.code
	let fightId = opts.fightId

	if (!code) {
		const dir = path.basename(path.dirname(inputPath))
		code = dir || ''
	}
	if (!fightId) {
		const match = path.basename(inputPath).match(/^fight_(\d+)_events\.json$/)
		if (match) {
			fightId = Number(match[1])
		}
	}

	if (!code || !fightId) {
		throw new Error('missing report code or fight id; pass --code and --fight-id')
	}
	return {code, fightId}
}

function loadEventsFile(filePath) {
	const raw = fs.readFileSync(filePath, 'utf8')
	let payload
	try {
		payload = JSON.parse(raw)
	} catch (err) {
		throw new Error(`invalid JSON: ${err.message}`)
	}

	if (Array.isArray(payload)) {
		return payload
	}
	if (payload && Array.isArray(payload.events)) {
		return payload.events
	}
	if (payload && payload.events && Array.isArray(payload.events.events)) {
		return payload.events.events
	}
	throw new Error('events JSON must be an array or {events:[...]}')
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
		if (sourceId != null) { ids.add(sourceId) }
		if (targetId != null) { ids.add(targetId) }
		if (Array.isArray(event.targets)) {
			for (const target of event.targets) {
				const tid = target.targetID ?? target.targetId
				if (tid != null) { ids.add(tid) }
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

function fetchReportData(token, code) {
	const query = `
	query ($code: String) {
		reportData {
			report(code: $code) {
				title
				startTime
				endTime
				fights {
					id
					name
					kill
					startTime
					endTime
					bossPercentage
					fightPercentage
					encounterID
					gameZone { id name }
				}
				masterData {
					actors {
						id
						name
						subType
						gameID
						type
					}
				}
			}
		}
	}
	`

	return graphQL(token, query, {code}).then(data => {
		const report = data.reportData && data.reportData.report
		if (!report) {
			throw new Error('reportData.report missing')
		}
		return report
	})
}

function graphQL(token, query, variables) {
	return fetchWithRetry('https://www.fflogs.com/api/v2/client', {
		method: 'POST',
		headers: {
			'Authorization': `Bearer ${token}`,
			'Content-Type': 'application/json',
		},
		body: JSON.stringify({query, variables}),
	}).then(resp => {
		if (!resp.ok) {
			return resp.text().then(text => {
				throw new Error(`GraphQL failed: ${resp.status} ${text}`)
			})
		}
		return resp.json()
	}).then(payload => {
		if (payload.errors) {
			throw new Error(`GraphQL errors: ${JSON.stringify(payload.errors)}`)
		}
		return payload.data
	})
}

function getAccessToken(clientId, clientSecret) {
	const auth = Buffer.from(`${clientId}:${clientSecret}`).toString('base64')
	return fetchWithRetry('https://www.fflogs.com/oauth/token', {
		method: 'POST',
		headers: {
			'Authorization': `Basic ${auth}`,
			'Content-Type': 'application/x-www-form-urlencoded',
		},
		body: 'grant_type=client_credentials',
	}).then(resp => {
		if (!resp.ok) {
			return resp.text().then(text => {
				throw new Error(`token failed: ${resp.status} ${text}`)
			})
		}
		return resp.json()
	}).then(payload => {
		if (!payload.access_token) {
			throw new Error('token missing access_token')
		}
		return payload.access_token
	})
}

function fetchWithRetry(url, options) {
	const attempts = readIntEnv('FFLOGS_FETCH_RETRY', 3)
	const timeoutMs = readIntEnv('FFLOGS_FETCH_TIMEOUT_MS', 20000)
	let lastErr

	const run = async (attempt) => {
		const controller = new AbortController()
		const timeout = setTimeout(() => controller.abort(), timeoutMs)
		try {
			return await fetch(url, {
				...options,
				signal: controller.signal,
			})
		} catch (err) {
			lastErr = err
			if (attempt >= attempts) {
				throw err
			}
			const waitMs = Math.min(5000, attempt * 1000)
			console.warn(`[net] fetch failed (attempt ${attempt}/${attempts}), retrying in ${waitMs}ms: ${err.message || err}`)
			await sleep(waitMs)
			return run(attempt + 1)
		} finally {
			clearTimeout(timeout)
		}
	}

	return run(1).catch(err => {
		throw lastErr || err
	})
}

function readIntEnv(key, fallback) {
	const raw = process.env[key]
	if (!raw) {
		return fallback
	}
	const val = Number(raw)
	return Number.isFinite(val) && val > 0 ? val : fallback
}

function sleep(ms) {
	return new Promise(resolve => setTimeout(resolve, ms))
}

function writeJson(filePath, data) {
	fs.mkdirSync(path.dirname(filePath), {recursive: true})
	fs.writeFileSync(filePath, JSON.stringify(data, null, 2))
}

function findEnvPath() {
	const candidates = [
		path.resolve(repoRoot, '.env'),
		path.resolve(xivaRoot, '.env'),
		path.resolve(repoRoot, 'external', '.env'),
	]
	return candidates.find(p => fs.existsSync(p))
}

function loadEnvFile(filePath) {
	if (!filePath) {
		return
	}
	const raw = fs.readFileSync(filePath, 'utf8')
	for (const line of raw.split('\n')) {
		const trimmed = line.trim()
		if (!trimmed || trimmed.startsWith('#')) {
			continue
		}
		const idx = trimmed.indexOf('=')
		if (idx === -1) {
			continue
		}
		const key = trimmed.slice(0, idx).trim()
		let value = trimmed.slice(idx + 1).trim()
		value = value.replace(/^"|"$/g, '')
		if (key && process.env[key] === undefined) {
			process.env[key] = value
		}
	}
}

function loadModules() {
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

	return {
		getEncounterKey: encounters.getEncounterKey,
		GameEdition: editions.GameEdition,
		AVAILABLE_MODULES: available.AVAILABLE_MODULES,
		Parser: parser.Parser,
		adaptEvents: adapter.adaptEvents,
		Team: report.Team,
	}
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
