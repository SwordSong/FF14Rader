global.window = global;
global.localStorage = { getItem: () => null, setItem: () => {}, removeItem: () => {} };
global.window.location = { search: '' };
global.window.addEventListener = () => {};
global.window.matchMedia = () => ({ matches: false, addListener: () => {} });
global.document = { addEventListener: () => {} };

require.extensions['.css'] = (module) => { 
  const proxy = new Proxy({}, { 
    get: (target, prop) => {
      if (prop === '__esModule') return true;
      if (prop === 'default') return proxy;
      if (typeof prop === 'string') {
          if (['theme', 'black', 'white', 'grey', 'red', 'orange', 'yellow', 'olive', 'green', 'teal', 'blue', 'violet', 'purple', 'pink', 'brown', 'bronze', 'background', 'text', 'highlight', 'blur', 'success', 'warning', 'error', 'info', 'primary', 'secondary'].some(c => prop.toLowerCase().includes(c))) return "blue";
          return "16px";
      }
      return target[prop];
    }
  }); 
  module.exports = proxy;
};
require.extensions['.png'] = (module) => { module.exports = ''; };
require.extensions['.jpg'] = (module) => { module.exports = ''; };

require(process.cwd() + '/node_modules/@babel/register')({
    extensions: ['.js', '.jsx', '.ts', '.tsx'],
    ignore: [/node_modules/]
});

const Parser = require(process.cwd() + '/src/parser/core/Parser').Parser;
const { ENCOUNTERS, getEncounterKey } = require(process.cwd() + '/src/data/ENCOUNTERS');
const { AVAILABLE_MODULES } = require(process.cwd() + '/src/parser/AVAILABLE_MODULES');
const { GameEdition } = require(process.cwd() + '/src/data/EDITIONS');
const { Team } = require(process.cwd() + '/src/report');
const { adaptEvents } = require(process.cwd() + '/src/reportSources/legacyFflogs/eventAdapter/adapter');

console.log('Parser:', !!Parser);
console.log('ENCOUNTERS Size:', Object.keys(ENCOUNTERS).length);
console.log('AVAILABLE_MODULES core modules:', !!AVAILABLE_MODULES.CORE);
console.log('GameEdition.GLOBAL:', GameEdition.GLOBAL);
console.log('Team.FRIEND:', Team.FRIEND);
console.log('adaptEvents:', !!adaptEvents);
