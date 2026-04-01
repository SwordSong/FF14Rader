const { chromium } = require('playwright');
(async () => {
    const browser = await chromium.launch({headless: true});
    const page = await browser.newPage();
    await page.goto('http://127.0.0.1:3001', {waitUntil: 'networkidle'});
    const keys = await page.evaluate(() => Object.keys(window).filter(k => k.includes('webpack') || k.includes('xivanalysis')));
    console.log('Keys:', keys);
    await browser.close();
})();
