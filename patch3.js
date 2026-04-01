const fs = require('fs');
let code = fs.readFileSync('scripts/xivanalysis-playwright.js', 'utf8');

code = code.replace("const encountersId = findModule('1083') || findModule('EX_TRAIN');\n\tconst encounters = req(encountersId);\n\tconsole.log(`[debug] encounters keys=${Object.keys(encounters)}`); //\n\n\tconst encounters = req(encountersId)", 
`const encountersId = findModule('1083') || findModule('EX_TRAIN');
const encounters = req(encountersId);
console.log("[debug] encounters=" + Object.keys(encounters));`);

code = code.replace("getEncounterKey: encounters.getEncounterKey,", "getEncounterKey: encounters.getEncounterKey || Object.values(encounters).find(x => typeof x === 'function'),");
code = code.replace("AVAILABLE_MODULES: available.AVAILABLE_MODULES,", "AVAILABLE_MODULES: available.AVAILABLE_MODULES || Object.values(available).find(x => typeof x === 'object' && x.CORE),");
code = code.replace("Parser: Parser,", "Parser: typeof Parser === 'function' ? Parser : Object.values(Parser).find(x => typeof x === 'function' && x.name === 'Parser'),");
code = code.replace("Team: reportMod.Team,", "Team: reportMod.Team || Object.values(reportMod).find(x => typeof x === 'object' && x.FRIEND === 1),");
code = code.replace("GameEdition: editions.GameEdition,", "GameEdition: editions.GameEdition || Object.values(editions).find(x => typeof x === 'object' && x.GLOBAL === 0),");

fs.writeFileSync('scripts/xivanalysis-playwright.js', code);
