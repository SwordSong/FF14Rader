const fs = require('fs');
let code = fs.readFileSync('scripts/xivanalysis-playwright.js', 'utf8');

const helpers = `
function serializeResults(results) {
return results.map(result => ({
handle: result.handle,
name: result.name && result.name.id ? result.name.id : result.handle,
mode: result.mode,
order: result.order,
}))
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
const encounterKey = fight.encounterID != null ? mods.getEncounterKey('legacyFflogs', String(fight.encounterID)) : undefined
const actors = report.masterData.actors.filter(a => !actorIdsInFight || actorIdsInFight.has(a.id)).map(a => buildActor(mods, a))
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
if (!subType || typeof subType !== 'string') return 'UNKNOWN'
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
`;

code = code.replace("const code = payload.code || 'unknown'", helpers + "\n\tconst code = payload.code || 'unknown'");
fs.writeFileSync('scripts/xivanalysis-playwright.js', code);
