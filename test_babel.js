require.extensions['.css'] = (module) => { 
  const proxy = new Proxy({}, { 
    get: (target, prop) => {
      if (prop === '__esModule') return true;
      if (prop === 'default') return proxy;
      if (typeof prop === 'string') {
          // If it starts with a color name or looks like it could trigger color() fail, we just return prop.
          // Better yet, just return prop and if they call replace we return it.
          // Wait, 'gutter' will fail extractNumber if it isn't a number string?
          // extractNumber does replace(/[^0-9]/g, ''), so "gutter" becomes "" which might be NaN and they do parseInt, maybe it fails.
          // Let's just return "100" for everything except known color strings!
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
const p = require(process.cwd() + '/src/parser/core/Parser');
console.log(p.Parser !== undefined ? 'Parser found!' : 'Failed');
