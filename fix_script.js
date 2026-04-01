const fs = require('fs');

let file = fs.readFileSync('scripts/xivanalysis-playwright.js', 'utf8');

// Add page.on('console')
if (!file.includes("page.on('console'")) {
    file = file.replace(
        "await page.goto(baseUrl",
        "page.on('console', msg => process.stdout.write(`[browser] ${msg.text()}\\n`));\nawait page.goto(baseUrl"
    );
}

// Remove the debug code
file = file.replace(
    "console.log('Encounters keys:', Object.keys(encounters).map(k=> typeof encounters[k])); throw new Error('stop here');\n        const code = payload.code || 'unknown'",
    "const code = payload.code || 'unknown'"
);

fs.writeFileSync('scripts/xivanalysis-playwright.js', file);
