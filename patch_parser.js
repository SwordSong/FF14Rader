const fs = require('fs');
let code = fs.readFileSync('scripts/xivanalysis-playwright.js', 'utf8');

const parserLogic = `
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
const parsedResults = parser.generateResults()
resultsByActor.push({
actorId: actor.id,
name: actor.name,
job: actor.job,
modules: serializeResults(parsedResults),
})
} catch (e) {
console.log('Error parsing actor ' + actor.id + ' : ' + e.message)
}
}

return {
status: 'ok',
reportCode: code,
fightId: fight.id,
fightName: fight.name,
startTime: report.startTime + fight.startTime,
endTime: report.startTime + fight.endTime,
actors: resultsByActor,
}
`;

code = code.replace("return {status: 'ok', fightId: payload.fightId}", parserLogic);
fs.writeFileSync('scripts/xivanalysis-playwright.js', code);
