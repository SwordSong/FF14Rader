const fs = require('fs');
let code = fs.readFileSync('scripts/xivanalysis-playwright.js', 'utf8');

code = code.replace("const encountersId = findModule('data/ENCOUNTERS');", "const encountersId = findModule('1083') || findModule('EX_TRAIN');");
code = code.replace("const editions = req(findModule('data/EDITIONS'))", "const editions = req(findModule('GameEdition') || findModule('CHINESE'))");
code = code.replace("const available = req(findModule('parser/AVAILABLE_MODULES'))", "const available = req(findModule('GUNBREAKER') || findModule('PICTOMANCER'))");
code = code.replace("const adapterIdx = findModule('eventAdapter/adapter') || findModule('reportSources/legacyFflogs/eventAdapter/adapter')", "const adapterIdx = findModule('dangerouslyMutatedBaseEvent')");
code = code.replace("const reportMod = req(findModule('report'))", "const reportMod = req(findModule('\"FRIEND\"') || findModule('FRIEND,') || findModule('FRIEND:'))");
code = code.replace("const Parser = req(findModule('parser/core/Parser')).Parser", "const Parser = Object.values(req(findModule('generateResults'))).find(x => typeof x === 'function' && x.name === 'Parser') || req(findModule('generateResults')).Parser;");

fs.writeFileSync('scripts/xivanalysis-playwright.js', code);
