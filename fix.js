const fs = require('fs');
let code = fs.readFileSync('scripts/xivanalysis-fast-parser.js', 'utf8');
code = code.replace(
  "function parseArgs(argv) { return {inDir: argv[1], outDir: argv[3] || argv[1], concurrency: argv[5] || 16} }",
  "function parseArgs(argv) { let r={}; for(let i=0;i<argv.length;i++){ if(argv[i]==='--in-dir') r.inDir=argv[++i]; if(argv[i]==='--out-dir') r.outDir=argv[++i]; if(argv[i]==='--concurrency') r.concurrency=argv[++i]; } return r; }"
);
fs.writeFileSync('scripts/xivanalysis-fast-parser.js', code);
