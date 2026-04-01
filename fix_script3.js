const fs = require('fs');
let file = fs.readFileSync('scripts/xivanalysis-playwright.js', 'utf8');

file = file.replace(
    "console.log('Encounters props:', Object.getOwnPropertyNames(encounters)); throw new Error('stop here!');",
    "const eid = findModule('EX_TRAIN'); const fac = req.m[eid].toString(); console.log('EX_TRAIN factory length:', fac.length); console.log(fac.substring(0, 500)); throw new Error('stop here!');"
);

fs.writeFileSync('scripts/xivanalysis-playwright.js', file);
