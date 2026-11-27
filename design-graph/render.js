// Renders every SVG in design-graph/ (and evolution/) to a 2x PNG beside it.
//   node design-graph/render.js
// Needs Playwright with Chromium. Fonts load from Google Fonts.
const fs = require('fs');
const path = require('path');
const { chromium } = require('playwright');

const root = __dirname;
const files = [
  ...fs.readdirSync(root).filter((f) => f.endsWith('.svg')).map((f) => path.join(root, f)),
  ...fs.readdirSync(path.join(root, 'evolution')).filter((f) => f.endsWith('.svg')).map((f) => path.join(root, 'evolution', f)),
];

(async () => {
  const browser = await chromium.launch();
  const page = await browser.newPage({ deviceScaleFactor: 2 });
  for (const file of files) {
    const svg = fs.readFileSync(file, 'utf8');
    const [, w, h] = svg.match(/viewBox="0 0 (\d+) (\d+)"/);
    await page.setViewportSize({ width: +w, height: +h });
    await page.setContent(
      '<!doctype html><link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Source+Sans+3:wght@400;700&display=swap">' +
        '<body style="margin:0">' + svg,
    );
    await page.evaluate(() => document.fonts.ready);
    await page.waitForTimeout(300);
    await (await page.$('svg')).screenshot({ path: file.replace(/\.svg$/, '.png') });
    console.log(file.replace(/\.svg$/, '.png'));
  }
  await browser.close();
})();
