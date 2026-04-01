const { chromium } = require('playwright');
(async () => {
    const browser = await chromium.launch({headless: true});
    const page = await browser.newPage();
    await page.goto('http://localhost:3001', {waitUntil: 'domcontentloaded'});
    await page.waitForTimeout(2000);
    const keys = await page.evaluate(() => Object.keys(window).filter(k => k.includes('webpack') || k.includes('xivanalysis') || k.includes('xiva')));
    console.log('Keys:', keys);
    await browser.close();
})();
