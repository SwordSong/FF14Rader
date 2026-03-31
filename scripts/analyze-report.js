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

async function main() {
	const opts = parseArgs(process.argv.slice(2))
	loadEnvFile(findEnvPath())

	const mods = loadModules()

	const clientId = process.env.FFLOGS_CLIENT_ID
	const clientSecret = process.env.FFLOGS_CLIENT_SECRET
	if (!clientId || !clientSecret) {
		throw new Error('missing FFLOGS_CLIENT_ID or FFLOGS_CLIENT_SECRET')
	}

	console.log('auth: requesting token')
	const token = await getAccessToken(clientId, clientSecret)
	console.log('auth: ok')

	console.log('report: fetching metadata')
	const report = await fetchReportData(token, opts.code)
	const fight = report.fights.find(f => f.id === opts.fightId)
	if (!fight) {
		throw new Error(`fight ${opts.fightId} not found in report ${opts.code}`)
	}

	const useV1 = opts.useV1 || (!opts.useV2 && !!process.env.FFLOGS_V1_API_KEY)
	console.log(`events: fetching (${useV1 ? 'v1' : 'v2'})`)
	const rawEvents = useV1
		? await fetchAllEventsV1(opts.code, fight)
		: await fetchAllEvents(token, opts.code, fight)
	console.log(`events: ${rawEvents.length} rows`)

	const events = useV1 ? rawEvents : normalizeV2Events(rawEvents)
	if (!useV1) {
		const withAbility = events.filter(event => event.ability && event.ability.guid != null).length
		console.log(`events: normalized with ability ${withAbility}/${events.length}`)
	}

	const actorIdsInFight = collectActorIds(events)
	const pull = buildPull(mods, report, fight, actorIdsInFight)
	const xvReport = buildReport(mods, opts.code, report, [pull])

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
		reportCode: opts.code,
		fightId: fight.id,
		fightName: fight.name,
		startTime: report.startTime + fight.startTime,
		endTime: report.startTime + fight.endTime,
		actors: resultsByActor,
	}

	writeJson(opts.outPath, output)
	console.log(`written: ${opts.outPath}`)
}

function parseArgs(args) {
	let code = 'pLCQ7Vz3ndAWN92G'
	let fightId = 1
	let outPath = path.resolve(repoRoot, 'downloads', 'fflogs', 'pLCQ7Vz3ndAWN92G', 'fight_1_modules.json')
	let useV1 = false
	let useV2 = false

	for (let i = 0; i < args.length; i++) {
		const arg = args[i]
		if (arg === '--code' && args[i + 1]) {
			code = args[i + 1]
			i++
		} else if (arg === '--fight-id' && args[i + 1]) {
			fightId = Number(args[i + 1])
			i++
		} else if (arg === '--out' && args[i + 1]) {
			outPath = args[i + 1]
			i++
		} else if (arg === '--use-v1') {
			useV1 = true
		} else if (arg === '--use-v2') {
			useV2 = true
		}
	}

	return {code, fightId, outPath, useV1, useV2}
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
			start: data.startTime,
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

function normalizeV2Events(events) {
	return events.map(event => normalizeV2Event(event))
}

function normalizeV2Event(event) {
	const next = {...event}

	if (next.sourceID == null) {
		next.sourceID = next.sourceId ?? next.source?.id ?? next.source?.ID
	}
	if (next.targetID == null) {
		next.targetID = next.targetId ?? next.target?.id ?? next.target?.ID
	}
	const abilityInfo = normalizeAbility(next)
	if (abilityInfo) {
		next.ability = abilityInfo
	}

	return next
}

function normalizeAbility(event) {
	const ability = event.ability
	const nameFromEvent = event.abilityName ?? event.actionName
	const iconFromEvent = event.abilityIcon ?? ''
	const typeFromEvent = event.abilityType ?? 0
	const guidFromEvent = event.abilityGameID ?? event.abilityID ?? event.abilityId

	if (ability == null) {
		if (guidFromEvent == null && nameFromEvent == null) {
			return undefined
		}
		return {
			guid: guidFromEvent ?? 0,
			name: nameFromEvent ?? `unknown_${guidFromEvent ?? '0'}`,
			abilityIcon: iconFromEvent,
			type: typeFromEvent,
		}
	}

	if (typeof ability === 'number') {
		return {
			guid: ability,
			name: nameFromEvent ?? `unknown_${ability}`,
			abilityIcon: iconFromEvent,
			type: typeFromEvent,
		}
	}

	if (typeof ability === 'string') {
		return {
			guid: guidFromEvent ?? 0,
			name: ability,
			abilityIcon: iconFromEvent,
			type: typeFromEvent,
		}
	}

	if (typeof ability === 'object') {
		const guid = ability.guid ?? ability.id ?? ability.gameID ?? ability.gameId ?? guidFromEvent
		return {
			guid: guid ?? 0,
			name: ability.name ?? nameFromEvent ?? `unknown_${guid ?? '0'}`,
			abilityIcon: ability.abilityIcon ?? ability.icon ?? iconFromEvent,
			type: ability.type ?? typeFromEvent,
		}
	}

	return undefined
}

function buildActor(mods, actor) {
	const isPlayer = actor.type === 'Player'
	return {
		id: String(actor.id),
		kind: actor.gameID != null ? String(actor.gameID) : String(actor.id),
		name: actor.name,
		team: isPlayer ? mods.Team.FRIEND : mods.Team.FOE,
		playerControlled: isPlayer,
		job: actor.subType || 'UNKNOWN',
	}
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

function fetchAllEvents(token, code, fight) {
	const query = `
	query ($code: String, $start: Float, $end: Float, $fids: [Int], $limit: Int) {
		reportData {
			report(code: $code) {
				events(startTime: $start, endTime: $end, fightIDs: $fids, limit: $limit) {
					data
					nextPageTimestamp
				}
			}
		}
	}
	`

	const limit = 5000
	let start = fight.startTime
	const end = fight.endTime
	const all = []

	let page = 0
	const loop = () => graphQL(token, query, {
		code,
		start,
		end,
		fids: [fight.id],
		limit,
	}).then(data => {
		const eventsBlock = data.reportData && data.reportData.report && data.reportData.report.events
		const rows = (eventsBlock && eventsBlock.data) || []
		all.push(...rows)
		page += 1
		console.log(`events: page ${page}, rows ${rows.length}, total ${all.length}`)

		const next = eventsBlock && eventsBlock.nextPageTimestamp
		if (!next || rows.length === 0) {
			return all
		}
		if (next <= start || next >= end) {
			return all
		}
		start = next
		return loop()
	})

	return loop()
}

function fetchAllEventsV1(code, fight) {
	const apiKey = process.env.FFLOGS_V1_API_KEY
	if (!apiKey) {
		throw new Error('missing FFLOGS_V1_API_KEY for v1 events')
	}

	let start = fight.startTime
	const end = fight.endTime
	const all = []
	let page = 0

	const loop = () => {
		const url = new URL(`https://www.fflogs.com/v1/report/events/${code}`)
		url.searchParams.set('api_key', apiKey)
		url.searchParams.set('start', String(start))
		url.searchParams.set('end', String(end))
		url.searchParams.set('translate', 'true')

		return fetch(url.toString())
			.then(resp => {
				if (!resp.ok) {
					return resp.text().then(text => {
						throw new Error(`v1 events failed: ${resp.status} ${text}`)
					})
				}
				return resp.json()
			})
			.then(data => {
				const rows = data.events || []
				all.push(...rows)
				page += 1
				console.log(`events: page ${page}, rows ${rows.length}, total ${all.length}`)

				const next = data.nextPageTimestamp
				if (!next || rows.length === 0) {
					return all
				}
				start = next
				return loop()
			})
	}

	return loop()
}

function graphQL(token, query, variables) {
	return fetch('https://www.fflogs.com/api/v2/client', {
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
	return fetch('https://www.fflogs.com/oauth/token', {
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

function registerAliases(srcPath) {
	const aliasRoots = new Set([
		'parser',
		'data',
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
