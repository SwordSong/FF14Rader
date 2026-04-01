const fs = require('fs');
let code = fs.readFileSync('scripts/xivanalysis-playwright.js', 'utf8');
code = code.replace("const code = payload.code || 'unknown'", "console.log('Encounters keys:', Object.keys(encounters).map(k=> typeof encounters[k])); throw new Error('stop here');\n\tconst code = payload.code || 'unknown'");
fs.writeFileSync('scripts/xivanalysis-playwright.js', code);
