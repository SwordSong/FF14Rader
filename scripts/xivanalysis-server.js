#!/usr/bin/env node
// Minimal HTTP stub for xivanalysis integration.
// Accepts POST /analyze with body {fights:[{id, data}]} and returns per-fight summaries.
// Built-in http only; no external deps. Use PORT env (default 3000).

const http = require('http');

const PORT = Number(process.env.PORT || process.env.port || 3000) || 3000;
const MAX_BODY_BYTES = Number(process.env.MAX_BODY_BYTES || 10 * 1024 * 1024) || 10 * 1024 * 1024; // 10MB default
const EVENTS_PREVIEW_LIMIT = Number(process.env.EVENTS_PREVIEW_LIMIT || 5) || 5;

function parseJSONSafe(buf) {
  try {
    return { ok: true, value: JSON.parse(buf) };
  } catch (err) {
    return { ok: false, error: err.message };
  }
}

function pickMeta(payload, id) {
  const fightId = payload.fight_id || payload.fightId || payload.id || id || '';
  const start = payload.start || payload.start_time || payload.startTime || null;
  const end = payload.end || payload.end_time || payload.endTime || null;
  return { fight_id: fightId, start, end };
}

function buildEventsPreview(events, limit) {
  if (!Array.isArray(events)) return [];
  return events.slice(0, limit).map((ev) => ({
    type: ev.type,
    timestamp: ev.timestamp || ev.time || ev.t,
    source: ev.source || ev.sourceID || ev.sourceId,
    target: ev.target || ev.targetID || ev.targetId,
    ability: (ev.ability && (ev.ability.name || ev.ability.guid)) || ev.abilityName,
    amount: ev.amount || ev.value || ev.damage || ev.heal,
  }));
}

function buildTablesPreview(tables) {
  if (!tables || typeof tables !== 'object') return [];
  return Object.entries(tables).map(([name, val]) => {
    if (Array.isArray(val)) {
      return {
        name,
        kind: 'array',
        length: val.length,
        sample: val.length > 0 ? val[0] : null,
      };
    }
    if (val && typeof val === 'object') {
      return {
        name,
        kind: 'object',
        keys: Object.keys(val).slice(0, 5),
      };
    }
    return { name, kind: typeof val, value: val };
  });
}

function normalizeFightPayload(fight) {
  const id = fight && fight.id ? String(fight.id) : '';
  const raw = fight ? fight.data : null;

  let payload = raw;
  if (typeof raw === 'string') {
    const parsed = parseJSONSafe(raw);
    if (!parsed.ok) return { id, error: `invalid JSON string: ${parsed.error}` };
    payload = parsed.value;
  }

  // If data is absent or not an object/array, bail out.
  if (payload === null || payload === undefined) {
    return { id, error: 'missing data' };
  }

  // If payload was provided as Buffer (rare), try parsing as utf8 JSON.
  if (Buffer.isBuffer(payload)) {
    const parsed = parseJSONSafe(payload.toString('utf8'));
    if (!parsed.ok) return { id, error: `invalid buffer JSON: ${parsed.error}` };
    payload = parsed.value;
  }

  if (typeof payload !== 'object') {
    return { id, error: `unexpected data type: ${typeof payload}` };
  }

  // Extract tables and events from known shapes.
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
    meta: pickMeta(payload, id),
    top_level_keys: payload && typeof payload === 'object' ? Object.keys(payload).slice(0, 10) : [],
    tables_keys: tables && typeof tables === 'object' ? Object.keys(tables) : [],
    tables_preview: buildTablesPreview(tables),
    event_count: Array.isArray(events) ? events.length : 0,
    events_preview: buildEventsPreview(events, EVENTS_PREVIEW_LIMIT),
  };

  return { id, summary, status: 'ok' };
}

function handleAnalyze(req, res, body) {
  const parsed = parseJSONSafe(body);
  if (!parsed.ok) {
    res.writeHead(400, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ error: `invalid JSON: ${parsed.error}` }));
    return;
  }

  const payload = parsed.value || {};
  const fights = Array.isArray(payload.fights) ? payload.fights : [];

  if (fights.length === 0) {
    res.writeHead(400, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ error: 'fights must be a non-empty array' }));
    return;
  }

  const results = fights.map((fight, idx) => {
    const normalized = normalizeFightPayload(fight);
    if (normalized.error) {
      return {
        id: normalized.id || `fight-${idx + 1}`,
        status: 'error',
        error: normalized.error,
      };
    }
    return {
      id: normalized.id || `fight-${idx + 1}`,
      status: normalized.status || 'ok',
      summary: normalized.summary,
    };
  });

  res.writeHead(200, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify({ results }));
}

function startServer() {
  const server = http.createServer((req, res) => {
    if (req.method !== 'POST' || req.url !== '/analyze') {
      res.writeHead(404, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: 'not found' }));
      return;
    }

    let body = Buffer.alloc(0);
    req.on('data', (chunk) => {
      body = Buffer.concat([body, chunk]);
      if (body.length > MAX_BODY_BYTES) {
        res.writeHead(413, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ error: 'payload too large' }));
        req.destroy();
      }
    });

    req.on('end', () => handleAnalyze(req, res, body));
    req.on('error', (err) => {
      res.writeHead(500, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: `request error: ${err.message}` }));
    });
  });

  server.listen(PORT, () => {
    console.log(`xivanalysis stub server listening on http://localhost:${PORT}`);
  });
}

startServer();
