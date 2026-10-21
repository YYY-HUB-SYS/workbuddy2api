// 校验：给定窗口宽度下，日志最长行是否完整显示（无横向滚动）
const path = require('path');
const puppeteer = require('puppeteer-core');
const CHROME = process.env.CHROME_PATH || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const FILE = 'file:///' + path.resolve(__dirname, 'ui-preview.html').replace(/\\/g, '/');

const CANDIDATES = [853, 855, 860, 865, 870];
// 日志里最长的一行（116 字符）
const LONGEST = '| #016 | 01:22:04 | global:deep | stream | 200 | uid=1a2b3c4d | TTFB=2135ms | tok=12018 | 232.4tok/s | total=51.7s |';

(async () => {
  const browser = await puppeteer.launch({
    executablePath: CHROME,
    headless: 'new',
    defaultViewport: { width: 1800, height: 1000 },
    args: ['--allow-file-access-from-files', '--force-device-scale-factor=1'],
    ignoreDefaultArgs: ['--hide-scrollbars'],
  });

  const rows = [];
  for (const w of CANDIDATES) {
    const page = await browser.newPage();
    await page.goto(FILE, { waitUntil: 'load' });
    await page.addStyleTag({ content: `:root{--w:${w}px !important}` });
    // 切到日志页，并把最长行塞进去，逼出最坏情况
    await page.click('#tab-lg');
    await page.evaluate((L) => {
      const t = document.querySelector('#lg .term');
      t.textContent = t.textContent + '\n' + L;
      t.scrollTop = t.scrollHeight;
    }, LONGEST);

    const r = await page.evaluate(() => {
      const t = document.querySelector('#lg .term');
      return {
        clientW: t.clientWidth,
        scrollW: t.scrollWidth,
        hOverflow: t.scrollWidth - t.clientWidth,
        hasVScroll: t.scrollHeight > t.clientHeight,
      };
    });
    rows.push({ width: w, ...r, fits: r.hOverflow <= 0 });
    await page.close();
  }
  console.table(rows);
  await browser.close();
})();
