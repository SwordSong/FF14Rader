#!/usr/bin/env node
const fs = require('fs');
const path = require('path');
const os = require('os');
const { Worker, isMainThread, parentPort, workerData } = require('worker_threads');

if (isMainThread) {
    const args = parseArgs(process.argv.slice(2));
    if (!args.inDir) process.exit(1);

    const inDir = path.resolve(args.inDir);
    const outDir = path.resolve(args.outDir || inDir);
    const concurrency = Math.max(1, Number(args.concurrency || os.cpus().length));

    fs.mkdirSync(outDir, {recursive: true});
    const requestFiles = fs.readdirSync(inDir).filter(n => /^fight_\d+_request\.json$/.test(n));

    if (requestFiles.length === 0) {
        console.log("No request files found.");
        process.exit(0);
    }

    let index = 0;
    let activeWorkers = 0;
    let hasErrors = false;

    function startWorker() {
        if (index >= requestFiles.length) {
            if (activeWorkers === 0) process.exit(hasErrors ? 1 : 0);
            return;
        }
        
        const file = requestFiles[index++];
        activeWorkers++;
        
        const worker = new Worker(__filename, {
            workerData: {
                inFile: path.join(inDir, file),
                outDir,
                file: file
            },
            env: { ...process.env, NODE_PATH: path.join(__dirname, '../external/xivanalysis/node_modules') + ':' + path.join(__dirname, '../external/xivanalysis/src') }
        });

        worker.on('message', (msg) => {
            if (msg.status === 'ok') console.log(`[ok] fight ${msg.fightId}`);
            if (msg.status === 'fail') {
                console.log(`[fail] fight ${msg.fightId}: ${msg.error}`);
                hasErrors = true;
            }
        });

        worker.on('error', (err) => {
            console.log(`[fail] Worker error: ${err.message}`);
            hasErrors = true;
            activeWorkers--;
            startWorker();
        });

        worker.on('exit', () => {
            activeWorkers--;
            startWorker();
        });
    }

    for (let i = 0; i < concurrency; i++) startWorker();

    function parseArgs(argv) { let r={}; for(let i=0;i<argv.length;i++){ if(argv[i]==='--in-dir') r.inDir=argv[++i]; if(argv[i]==='--out-dir') r.outDir=argv[++i]; if(argv[i]==='--concurrency') r.concurrency=argv[++i]; } return r; }
} else {
    // WORkER THREAD
    const cwd = path.join(__dirname, '../external/xivanalysis');
    // process.chdir(cwd);
    
    // POLYFILLS
    global.window = global;
    global.localStorage = { getItem: () => null, setItem: () => {}, removeItem: () => {} };
    global.window.location = { search: '', reload: () => {} };
    global.window.addEventListener = () => {};
    global.window.matchMedia = () => ({ matches: false, addListener: () => {} });
    global.document = { addEventListener: () => {} };
    global.requestAnimationFrame = (cb) => setTimeout(cb, 0);
    global.cancelAnimationFrame = (id) => clearTimeout(id);

    require.extensions['.css'] = (module) => { 
      const proxy = new Proxy({}, { 
        get: (target, prop) => {
          if (prop === '__esModule') return true;
          if (prop === 'default') return proxy;
          if (typeof prop === 'string') {
              if (['theme', 'black', 'white', 'grey', 'red', 'orange', 'yellow', 'olive', 'green', 'teal', 'blue', 'violet', 'purple', 'pink', 'brown', 'bronze', 'background', 'text', 'highlight', 'blur', 'success', 'warning', 'error', 'info', 'primary', 'secondary'].some(c => prop.toLowerCase().includes(c))) return "blue";
              return "16px";
          }
          return target[prop];
        }
      }); 
      module.exports = proxy;
    };
    require.extensions['.png'] = (module) => { module.exports = ''; };
    require.extensions['.jpg'] = (module) => { module.exports = ''; };

    require(cwd + '/node_modules/@babel/register')({
        extensions: ['.js', '.jsx', '.ts', '.tsx'],
        ignore: [/node_modules/]
    });

    const Parser = require(cwd + '/src/parser/core/Parser').Parser;
    const { ENCOUNTERS, getEncounterKey } = require(cwd + '/src/data/ENCOUNTERS');
    const { AVAILABLE_MODULES } = require(cwd + '/src/parser/AVAILABLE_MODULES');
    const { GameEdition } = require(cwd + '/src/data/EDITIONS');
    const { Team } = require(cwd + '/src/report');
    const { adaptEvents } = require(cwd + '/src/reportSources/legacyFflogs/eventAdapter/adapter');

    function serializeResults(results) { return results.map(r => ({ handle: r.handle, name: r.name && r.name.id ? r.name.id : r.handle, mode: r.mode, order: r.order })) }
    function buildReport(code, data, pulls) { return { timestamp: data.startTime, edition: GameEdition.GLOBAL, name: data.title, pulls, meta: { source: 'legacyFflogs', code, end: data.endTime, fights: data.fights, friendlies: [], enemies: [] } } }
    function collectActorIds(events) { const ids = new Set(); for (const ev of events) { if(ev.sourceID!=null) ids.add(ev.sourceID); if(ev.targetID!=null) ids.add(ev.targetID); if(Array.isArray(ev.targets)) { for(const t of ev.targets) if(t.targetID!=null) ids.add(t.targetID); } } return ids; }
    function buildPull(rep, fight, actorIds) { return { id: fight.id.toString(), timestamp: rep.startTime + fight.startTime, duration: fight.endTime - fight.startTime, encounter: { key: fight.encounterID != null ? getEncounterKey('legacyFflogs', String(fight.encounterID)) : undefined, name: fight.name, duty: { id: fight.gameZone ? fight.gameZone.id : 0, name: fight.gameZone ? fight.gameZone.name : 'Unknown' } }, actors: rep.masterData.actors.filter(a => !actorIds || actorIds.has(a.id)).map(a => ({ kind: a.gameID != null ? String(a.gameID) : String(a.id), name: a.name, team: a.type === 'Player' ? Team.FRIEND : Team.FOE, playerControlled: a.type === 'Player', job: (a.subType||'').trim().toUpperCase().replace(/[-\s]+/g, '_'), id: a.id })) } }
    function buildMeta(encKey, job) { let meta = AVAILABLE_MODULES.CORE; if(encKey && AVAILABLE_MODULES.BOSSES[encKey]) meta = meta.merge(AVAILABLE_MODULES.BOSSES[encKey]); if(AVAILABLE_MODULES.JOBS[job]) meta = meta.merge(AVAILABLE_MODULES.JOBS[job]); return meta; }

    try {
        const payload = JSON.parse(fs.readFileSync(workerData.inFile, 'utf8'));
        const code = payload.code || 'unknown';
        const report = payload.report;
        const fightId = payload.fightId || workerData.file;
        const fight = report.fights.find(f => Number(f.id) === Number(fightId));
        if (!fight) throw new Error('fight not found in report');

        const events = Array.isArray(payload.events) ? payload.events : payload.events.events;
        const actorIdsInFight = collectActorIds(events);
        const pull = buildPull(report, fight, actorIdsInFight);
        const xvReport = buildReport(code, report, [pull]);
        const firstEvent = report.startTime + fight.startTime;
        const adapted = adaptEvents(xvReport, pull, events, firstEvent);

        const resultsByActor = [];
        for (const actor of pull.actors) {
            if (actor.team !== Team.FRIEND || !actor.playerControlled) continue;
            try {
                const meta = buildMeta(pull.encounter.key, actor.job);
                const parser = new Parser({ meta, report: xvReport, pull, actor });
                parser.configure();
                parser.parseEvents({events: adapted});
                resultsByActor.push({ actorId: actor.id, name: actor.name, job: actor.job, modules: serializeResults(parser.generateResults()) });
            } catch (e) {
                // Ignore individual actor errors silently, just don't populate them or log them quietly
            }
        }
        
        fs.writeFileSync(path.join(workerData.outDir, `fight_${fightId}_analysis.json`), JSON.stringify({ status: 'ok', fightId: fight.id, actors: resultsByActor }, null, 2));
        parentPort.postMessage({status: 'ok', fightId: fight.id});
    } catch(err) {
        parentPort.postMessage({status: 'fail', fightId: workerData.file, error: err.stack});
    }
}
