const fs = require('fs');
let code = fs.readFileSync('scripts/xivanalysis-playwright.js', 'utf8');
code = code.replace("const encounters = req(encountersId);\n\tconst editions = req", "const editions = req");
fs.writeFileSync('scripts/xivanalysis-playwright.js', code);
