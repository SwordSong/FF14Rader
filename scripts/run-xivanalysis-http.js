#!/usr/bin/env node
// Run xivanalysis HTTP server against local fight event files and DB metadata.

const fs = require('fs')
const path = require('path')
const http = require('http')
const https = require('https')
const {execFileSync} = require('child_process')

const repoRoot = path.resolve(__dirname, '..')

const args = parseArgs(process.argv.slice(2))
if (!args.dir) {
	console.error('Usage: node scripts/run-xivanalysis-http.js --dir <report_dir> [--code <report>] [--fight-id <id>] [--api-url <url>] [--out-dir <dir>]')
	process.exit(1)
}

loadEnvFile(findEnvPath())

const reportDir = path.resolve(args.dir)
const reportCode = args.code || path.basename(reportDir)
const apiUrl = args.apiUrl || 'http://localhost:3000/analyze'
const outDir = path.resolve(args.outDir || reportDir)

const reportFightsPath = path.join(reportDir, 'report_fights.json')
if (!fs.existsSync(reportFightsPath)) {
	console.error(`missing report_fights.json in ${reportDir}`)
	process.exit(1)
}

const reportFights = readJson(reportFightsPath)
const reportMeta = loadReportMetadata(reportCode)
const report = buildReportMetadata(reportCode, reportFights, reportMeta)

if (!report.masterData || !Array.isArray(report.masterData.actors)) {
	console.error('report.masterData.actors missing; ensure report_metadata exists in DB')
	process.exit(1)
}

const eventFiles = fs.readdirSync(reportDir)
	.filter(name => /^fight_\d+_events\.json$/.test(name))
	.sort(compareFightFiles)

if (eventFiles.length === 0) {
	console.error('no fight_#_events.json files found')
	process.exit(1)
}

fs.mkdirSync(outDir, {recursive: true})

main().catch(err => {
	console.error(err && err.stack ? err.stack : err)
	process.exit(1)
})

async function main() {
	for (const filename of eventFiles) {
		const fightId = extractFightId(filename)
		if (args.fightId && fightId !== args.fightId) {
			continue
		}

		const fight = report.fights.find(f => Number(f.id) === fightId)
		if (!fight) {
			console.warn(`[skip] fight ${fightId} missing in report metadata`)
			continue
		}

		const eventsPath = path.join(reportDir, filename)
		const events = readEvents(eventsPath)

		const payload = {
			code: reportCode,
			fightId,
			report,
			events,
		}

		console.log(`[request] fight ${fightId} (${events.length} events)`)
		const response = await postJson(apiUrl, payload)

		const outPath = path.join(outDir, `fight_${fightId}_analysis.json`)
		writeJson(outPath, response)
		console.log(`[written] ${outPath}`)
	}
}

function parseArgs(argv) {
	const out = {}
	for (let i = 0; i < argv.length; i++) {
		const arg = argv[i]
		if (arg === '--dir' && argv[i + 1]) {
			out.dir = argv[++i]
		} else if (arg === '--code' && argv[i + 1]) {
			out.code = argv[++i]
		} else if (arg === '--fight-id' && argv[i + 1]) {
			out.fightId = Number(argv[++i])
		} else if (arg === '--api-url' && argv[i + 1]) {
			out.apiUrl = argv[++i]
		} else if (arg === '--out-dir' && argv[i + 1]) {
			out.outDir = argv[++i]
		}
	}
	return out
}

function extractFightId(filename) {
	const match = filename.match(/^fight_(\d+)_events\.json$/)
	return match ? Number(match[1]) : 0
}

function compareFightFiles(a, b) {
	return extractFightId(a) - extractFightId(b)
}

function readEvents(filePath) {
	const payload = readJson(filePath)
	if (Array.isArray(payload)) return payload
	if (payload && Array.isArray(payload.events)) return payload.events
	if (payload && payload.events && Array.isArray(payload.events.events)) return payload.events.events
	throw new Error(`invalid events JSON shape: ${filePath}`)
}

function readJson(filePath) {
	const raw = fs.readFileSync(filePath, 'utf8')
	try {
		return JSON.parse(raw)
	} catch (err) {
		throw new Error(`invalid JSON in ${filePath}: ${err.message}`)
	}
}

function buildReportMetadata(code, reportFights, reportMeta) {
	const base = normalizeReportFights(code, reportFights)
	if (!reportMeta) {
		return base
	}

	const merged = {
		...reportMeta,
		code: reportMeta.code || code,
		startTime: reportMeta.startTime != null ? reportMeta.startTime : base.startTime,
		endTime: reportMeta.endTime != null ? reportMeta.endTime : base.endTime,
		fights: mergeFights(reportMeta.fights || [], base.fights || []),
	}

	if (!merged.masterData && base.masterData) {
		merged.masterData = base.masterData
	}

	return merged
}

function normalizeReportFights(code, data) {
	const fights = Array.isArray(data.fights) ? data.fights.map(f => ({
		id: f.id,
		name: f.name,
		kill: !!f.kill,
		startTime: f.start_time != null ? f.start_time : f.startTime,
		endTime: f.end_time != null ? f.end_time : f.endTime,
		fightPercentage: f.fightPercentage,
		bossPercentage: f.bossPercentage,
		encounterID: f.encounterID,
		difficulty: f.difficulty,
		gameZone: f.gameZone,
	})) : []

	return {
		code: code || data.code || '',
		title: data.title || '',
		startTime: data.start != null ? data.start : data.startTime,
		endTime: data.end != null ? data.end : data.endTime,
		fights,
		masterData: data.masterData,
	}
}

function mergeFights(fromMeta, fromFile) {
	const byId = new Map()
	for (const fight of fromFile) {
		byId.set(Number(fight.id), fight)
	}
	for (const fight of fromMeta) {
		const id = Number(fight.id)
		const existing = byId.get(id)
		if (!existing) {
			byId.set(id, fight)
			continue
		}
		byId.set(id, {
			...existing,
			...fight,
			startTime: fight.startTime != null ? fight.startTime : existing.startTime,
			endTime: fight.endTime != null ? fight.endTime : existing.endTime,
		})
	}
	return Array.from(byId.values())
}

function loadReportMetadata(code) {
	const dsn = process.env.POSTGRES_READ_DSN || process.env.POSTGRES_WRITE_DSN || process.env.DATABASE_URL
	if (!dsn) {
		return null
	}
	const sqlCode = code.replace(/'/g, "''")
	const sql = `select report_metadata from report_parse_logs where source_report='${sqlCode}' or master_report='${sqlCode}' order by updated_at desc nulls last limit 1;`
	try {
		const stdout = execFileSync('psql', [dsn, '-t', '-A', '-c', sql], {encoding: 'utf8'})
		const trimmed = stdout.trim()
		if (!trimmed) {
			return null
		}
		return JSON.parse(trimmed)
	} catch (err) {
		console.warn(`[warn] load report_metadata failed: ${err.message || err}`)
		return null
	}
}

function postJson(urlStr, payload) {
	const url = normalizeApiUrl(urlStr)
	const body = JSON.stringify(payload)
	const isHttps = url.protocol === 'https:'
	const options = {
		method: 'POST',
		hostname: url.hostname,
		port: url.port || (isHttps ? 443 : 80),
		path: url.pathname,
		headers: {
			'Content-Type': 'application/json',
			'Content-Length': Buffer.byteLength(body),
		},
	}

	return new Promise((resolve, reject) => {
		const req = (isHttps ? https : http).request(options, res => {
			const chunks = []
			res.on('data', chunk => chunks.push(chunk))
			res.on('end', () => {
				const raw = Buffer.concat(chunks).toString('utf8')
				if (res.statusCode && res.statusCode >= 400) {
					return reject(new Error(`HTTP ${res.statusCode}: ${raw}`))
				}
				try {
					resolve(JSON.parse(raw))
				} catch (_err) {
					resolve({raw})
				}
			})
		})
		req.on('error', reject)
		req.write(body)
		req.end()
	})
}

function normalizeApiUrl(urlStr) {
	const url = new URL(urlStr)
	if (!url.pathname || url.pathname === '/') {
		url.pathname = '/analyze'
	}
	return url
}

function writeJson(filePath, data) {
	fs.writeFileSync(filePath, JSON.stringify(data, null, 2))
}

function findEnvPath() {
	const candidates = [
		path.resolve(repoRoot, '.env'),
		path.resolve(repoRoot, 'external', '.env'),
	]
	return candidates.find(p => fs.existsSync(p))
}

function loadEnvFile(filePath) {
	if (!filePath) return
	const raw = fs.readFileSync(filePath, 'utf8')
	for (const line of raw.split('\n')) {
		const trimmed = line.trim()
		if (!trimmed || trimmed.startsWith('#')) continue
		const idx = trimmed.indexOf('=')
		if (idx === -1) continue
		const key = trimmed.slice(0, idx).trim()
		let value = trimmed.slice(idx + 1).trim()
		value = value.replace(/^"|"$/g, '')
		if (key && process.env[key] === undefined) {
			process.env[key] = value
		}
	}
}
