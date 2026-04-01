const fs = require('fs');

let file = fs.readFileSync('scripts/xivanalysis-playwright.js', 'utf8');

file = file.replace(
    "console.log('Encounters keys:', Object.keys(encounters).map(k=> typeof encounters[k])); throw new Error('stop here');",
    "console.log('Encounters props:', Object.getOwnPropertyNames(encounters)); throw new Error('stop here!');"
);

fs.writeFileSync('scripts/xivanalysis-playwright.js', file);
