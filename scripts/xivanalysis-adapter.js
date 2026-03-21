#!/usr/bin/env node
// Legacy CLI stub (prefer the HTTP server in scripts/xivanalysis-server.js).
// It reads the combined tables+events JSON file produced by Sync (FightCache.Data)
// and prints a simple summary. Replace with real xivanalysis integration later.

const fs = require('fs');
const path = require('path');

function parseArgs() {
  const args = process.argv.slice(2);
  const out = {};
  for (let i = 0; i < args.length; i++) {
    if (args[i] === '--input' && args[i + 1]) {
      out.input = args[i + 1];
      i++;
    }
  }
  if (!out.input) {
    console.error('Usage: node scripts/xivanalysis-adapter.js --input <file>');
    process.exit(1);
  }
  return out;
}

function main() {
  const { input } = parseArgs();
  const p = path.resolve(input);
  const raw = fs.readFileSync(p, 'utf8');

  let payload;
  try {
    payload = JSON.parse(raw);
  } catch (e) {
    console.error('Failed to parse JSON:', e.message);
    process.exit(1);
  }

  // The payload can be either plain tables JSON or wrapped {tables, events}
  let tables = payload;
  let events = [];
  if (payload && payload.tables) {
    tables = payload.tables;
  }
  if (payload && payload.events) {
    if (Array.isArray(payload.events.events)) {
      events = payload.events.events;
    } else if (Array.isArray(payload.events)) {
      events = payload.events;
    }
  }

  const summary = {
    fight_id: payload.fight_id || null,
    start: payload.start || null,
    end: payload.end || null,
    tables_keys: tables ? Object.keys(tables) : [],
    event_count: events.length,
    sample_events: events.slice(0, 3),
  };

  console.log(JSON.stringify({ summary }, null, 2));
}

main();
