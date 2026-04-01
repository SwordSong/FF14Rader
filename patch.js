const fs = require('fs');
let code = fs.readFileSync('scripts/xivanalysis-playwright.js', 'utf8');

// Find start and end of runAnalysis
let lines = code.split('\n');
let runAnalysisStart = lines.findIndex(l => l.startsWith('function runAnalysis(payload) {'));
let runAnalysisEnd = lines.findIndex(l => l.startsWith('function getWebpackRequire() {')) - 1;

// Put all functions from getWebpackRequire to readJSON inside runAnalysis
let functionsToMoveStart = runAnalysisEnd + 1;
let functionsToMoveEnd = lines.findIndex(l => l.startsWith('function listRequestFiles'));

let toMove = lines.slice(functionsToMoveStart, functionsToMoveEnd).join('\n');
lines.splice(functionsToMoveStart, functionsToMoveEnd - functionsToMoveStart);

// insert before the end of runAnalysis
let insertPos = lines.findIndex(l => l.startsWith('function listRequestFiles')) - 2;
// Wait, runAnalysisEnd is where it ends with "} "
lines[runAnalysisEnd - 1] += '\n\n' + toMove;

fs.writeFileSync('scripts/xivanalysis-playwright.js', lines.join('\n'));
