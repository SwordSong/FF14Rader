const fs = require('fs');
let code = fs.readFileSync('scripts/xivanalysis-playwright.js', 'utf8');

const routeStr = `uest().url().startsWith(baseUrl)) {
return route.abort();
}
route.continue();
});`;

code = code.replace("page.setDefaultTimeout(evalTimeoutMs)", routeStr + "\n\t\tpage.setDefaultTimeout(evalTimeoutMs)");

fs.writeFileSync('scripts/xivanalysis-playwright.js', code);
