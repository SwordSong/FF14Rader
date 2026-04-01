const fs = require('fs');
let code = fs.readFileSync('scripts/xivanalysis-playwright.js', 'utf8');
code = code.replace("console.log(`[debug] encountersId=${encountersId}, m keys=${Object.keys(req.m).length}`);",
"const encounters = req(encountersId);\n\tconsole.log(`[debug] encounters keys=${Object.keys(encounters)}`); //");
fs.writeFileSync('scripts/xivanalysis-playwright.js', code);
