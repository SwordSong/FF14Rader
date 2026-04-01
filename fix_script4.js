const fs = require('fs');
let file = fs.readFileSync('scripts/xivanalysis-playwright.js', 'utf8');

file = file.replace(
    "const eid = findModule('EX_TRAIN'); const fac = req.m[eid].toString(); console.log('EX_TRAIN factory length:', fac.length); console.log(fac.substring(0, 500)); throw new Error('stop here!');",
    "console.log('Window keys:', Object.keys(window).filter(k => k.toLowerCase().includes('xiv'))); throw new Error();"
);

fs.writeFileSync('scripts/xivanalysis-playwright.js', file);
