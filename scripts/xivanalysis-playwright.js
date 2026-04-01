#!/usr/bin/env node
const fs = require('fs')
const path = require('path')
const {chromium} = require('playwright')

const args = parseArgs(process.argv.slice(2))
if (!args.inDir) process.exit(1)

const inDir = path.resolve(args.inDir)
const outDir = path.resolve(args.outDir || inDir)
const baseUrl = args.url || 'http://127.0.0.1:3001'
const concurrency = Math.max(1, Number(args.concurrency || 16))
const evalTimeoutMs = Math.max(1, Number(args.evalTimeoutMs || 120000))

const requestFiles = listRequestFiles(inDir)
fs.mkdirSync(outDir, {recursive: true})

main().catch(err => { console.error(err); process.exit(1) })

async function main() {
const browser = await chromium.launch({headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox']})
const pages = []
for (let i = 0; i < concurrency; i++) {
const page = await browser.newPage()
await page.route('**/*', route => {
if (!route.request().url().startsWith(baseUrl) && !route.request().url().startsWith('http://localhost:3001')) return route.abort();
route.continue();
});
page.setDefaultTimeout(evalTimeoutMs)
page.on('console', msg => process.stdout.write(`[browser] ${msg.text()}\n`));
await page.goto(baseUrl, {waitUntil: 'domcontentloaded', timeout: evalTimeoutMs})
await page.waitForFunction(() => typeof window !== 'undefined' && (window.__webpack_require__ !== undefined || window.webpackChunk_xivanalysis_client !== undefined || window.webpackChunkxivanalysis !== undefined || window.webpackChunk !== undefined || window.webpackChunk_xivanalysis !== undefined), {timeout: evalTimeoutMs})
await page.waitForTimeout(1000)
pages.push(page)
}

let index = 0, errors = 0
await Promise.all(pages.map(page => worker(page)))
await browser.close()
if (errors > 0) process.exit(1)

async function worker(page) {
while (true) {
const current = index++
if (current >= requestFiles.length) return
const file = requestFiles[current]
const payload = readJSON(path.join(inDir, file))
const fightId = payload && payload.fightId ? payload.fightId : file
try {
process.stdout.write(`[start] fight ${fightId}\n`)
const result = await page.evaluate(runAnalysis, payload)
fs.writeFileSync(path.join(outDir, `fight_${fightId}_analysis.json`), JSON.stringify(result, null, 2))
process.stdout.write(`[ok] fight ${fightId}\n`)
} catch (err) {
errors++
process.stdout.write(`[fail] fight ${fightId}: ${err.message || err}\n`)
}
}
}
}

function runAnalysis(payload) {
function serializeResults(results) { return results.map(r => ({ handle: r.handle, name: r.name && r.name.id ? r.name.id : r.handle, mode: r.mode, order: r.order })) }
function buildReport(mods, code, data, pulls) { return { timestamp: data.startTime, edition: mods.GameEdition.GLOBAL, name: data.title, pulls, meta: { source: 'legacyFflogs', code, end: data.endTime, fights: data.fights, friendlies: [], enemies: [] } } }
function buildPull(mods, rep, fight, actorIds) { return { id: fight.id.toString(), timestamp: rep.startTime + fight.startTime, duration: fight.endTime - fight.startTime, encounter: { key: fight.encounterID != null ? mods.getEncounterKey('legacyFflogs', String(fight.encounterID)) : undefined, name: fight.name, duty: { id: fight.gameZone ? fight.gameZone.id : 0, name: fight.gameZone ? fight.gameZone.name : 'Unknown' } }, actors: rep.masterData.actors.filter(a => !actorIds || actorIds.has(a.id)).map(a => ({ kind: a.gameID != null ? String(a.gameID) : String(a.id), name: a.name, team: a.type === 'Player' ? mods.Team.FRIEND : mods.Team.FOE, playerControlled: a.type === 'Player', job: (a.subType||'').trim().toUpperCase().replace(/[-\s]+/g, '_'), id: a.id })) } }
function collectActorIds(events) { const ids = new Set(); for (const ev of events) { if(ev.sourceID!=null) ids.add(ev.sourceID); if(ev.targetID!=null) ids.add(ev.targetID); if(Array.isArray(ev.targets)) { for(const t of ev.targets) if(t.targetID!=null) ids.add(t.targetID); } } return ids; }
function buildMeta(mods, encKey, job) { let meta = mods.AVAILABLE_MODULES.CORE; if(encKey && mods.AVAILABLE_MODULES.BOSSES[encKey]) meta = meta.merge(mods.AVAILABLE_MODULES.BOSSES[encKey]); if(mods.AVAILABLE_MODULES.JOBS[job]) meta = meta.merge(mods.AVAILABLE_MODULES.JOBS[job]); return meta; }

const req = (function() {
if (typeof window.__webpack_require__ === 'function') return window.__webpack_require__
const cg = window.webpackChunk_xivanalysis_client || window.webpackChunkxivanalysis || window.webpackChunk || window.webpackChunk_xivanalysis
if (!cg) return undefined
let r; cg.push([[Math.random()], {}, (req) => { r = req }]); return r
})();
if (!req || !req.m) throw new Error('webpack runtime not found')

const cache = window.__xivaModuleCache || (window.__xivaModuleCache = {})
const findModule = (marker) => {
if (cache[marker]) return cache[marker]
for (const id of Object.keys(req.m)) { if (req.m[id].toString().includes(marker)) { cache[marker] = id; return id } } return null
}

const encounters = req(findModule('1083') || findModule('EX_TRAIN'));
const editions = req(findModule('GameEdition') || findModule('CHINESE'));
const available = req(findModule('GUNBREAKER') || findModule('PICTOMANCER'));
const adapter = req(findModule('dangerouslyMutatedBaseEvent'));
const reportMod = req(findModule('"FRIEND"') || findModule('FRIEND,') || findModule('FRIEND:'));
const ParserMod = req(findModule('generateResults'));

const mods = {
getEncounterKey: encounters.getEncounterKey || Object.values(encounters).find(x => typeof x === 'function'),
GameEdition: editions.GameEdition || Object.values(editions).find(x => typeof x === 'object' && x.GLOBAL === 0),
AVAILABLE_MODULES: available.AVAILABLE_MODULES || Object.values(available).find(x => typeof x === 'object' && x.CORE),
Parser: typeof ParserMod.Parser === 'function' ? ParserMod.Parser : Object.values(ParserMod).find(x => typeof x === 'function' && x.name === 'Parser'),
adaptEvents: adapter.adaptEvents || Object.values(adapter).find(x => typeof x === 'function' && x.name === 'adaptEvents'),
Team: reportMod.Team || Object.values(reportMod).find(x => typeof x === 'object' && x.FRIEND === 1),
}

console.log('Window keys:', Object.keys(window).filter(k => k.toLowerCase().includes('xiv'))); throw new Error();
	const code = payload.code || 'unknown'
const report = payload.report
const fightId = payload.fightId
const fight = report.fights.find(f => Number(f.id) === Number(fightId))
if (!fight) throw new Error('fight not found in report')

const events = Array.isArray(payload.events) ? payload.events : payload.events.events
const actorIdsInFight = collectActorIds(events)
const pull = buildPull(mods, report, fight, actorIdsInFight)
const xvReport = buildReport(mods, code, report, [pull])
const firstEvent = report.startTime + fight.startTime
const adapted = mods.adaptEvents(xvReport, pull, events, firstEvent)

const resultsByActor = []
for (const actor of pull.actors) {
if (actor.team !== mods.Team.FRIEND || !actor.playerControlled) continue
try {
const meta = buildMeta(mods, pull.encounter.key, actor.job)
const parser = new mods.Parser({ meta, report: xvReport, pull, actor })
parser.configure()
parser.parseEvents({events: adapted})
resultsByActor.push({ actorId: actor.id, name: actor.name, job: actor.job, modules: serializeResults(parser.generateResults()) })
} catch (e) {
console.log('Error parsing actor ' + actor.id + ' : ' + e.message)
}
}
return { status: 'ok', fightId: fight.id, actors: resultsByActor }
}

function listRequestFiles(dir) { return fs.readdirSync(dir).filter(n => /^fight_\d+_request\.json$/.test(n)) }
function readJSON(p) { return JSON.parse(fs.readFileSync(p, 'utf8')) }
function parseArgs(argv) { return {inDir: argv[1], outDir: argv[3], url: argv[5], concurrency: 1} }
