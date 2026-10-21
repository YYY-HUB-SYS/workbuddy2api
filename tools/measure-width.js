// 测量：日志最长行像素宽度 + 真实滚动条宽度 → 反推窗口应有宽度
const path = require('path');
const puppeteer = require('puppeteer-core');

const CHROME = process.env.CHROME_PATH || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const FILE = 'file:///' + path.resolve(__dirname, 'ui-preview.html').replace(/\\/g, '/');

// 日志里最长的一行（116 字符，tok 为 5 位数）
const LONGEST = '| #016 | 01:22:04 | global:deep | stream | 200 | uid=1a2b3c4d | TTFB=2135ms | tok=12018 | 232.4tok/s | total=51.7s |';

(async () => {
  const browser = await puppeteer.launch({
    executablePath: CHROME,
    headless: 'new',
    defaultViewport: { width: 1600, height: 900 },
    args: ['--allow-file-access-from-files', '--force-device-scale-factor=1'],
    ignoreDefaultArgs: ['--hide-scrollbars'],
  });
  const page = await browser.newPage();
  await page.goto(FILE, { waitUntil: 'load' });
  await page.addStyleTag({ content: ':root{--w:2000px !important}' });

  const r = await page.evaluate((LONGEST) => {
    const term = document.querySelector('#lg .term');
    const cs = getComputedStyle(term);
    const n = v => parseFloat(v) || 0;

    // ── 1. 文本宽度（canvas + DOM 双测交叉验证）──
    const ctx = document.createElement('canvas').getContext('2d');
    ctx.font = `${cs.fontSize} ${cs.fontFamily}`;
    const canvasW = ctx.measureText(LONGEST).width;

    const probe = document.createElement('span');
    probe.style.cssText =
      'position:absolute;visibility:hidden;white-space:pre;' +
      `font-size:${cs.fontSize};font-family:${cs.fontFamily};`;
    document.body.appendChild(probe);
    probe.textContent = LONGEST;
    const domW = probe.getBoundingClientRect().width;
    probe.remove();

    const perChar = canvasW / LONGEST.length;

    // ── 2. 真实滚动条宽度：造一个必定溢出的同款容器来量 ──
    function sbWidth(extraCss, cls) {
      const d = document.createElement('div');
      d.className = cls;
      d.style.cssText =
        'position:absolute;top:-9999px;width:200px;height:50px;overflow-y:scroll;' + extraCss;
      d.innerHTML = '<div style="height:500px"></div>';
      document.body.appendChild(d);
      const w = d.offsetWidth - d.clientWidth;
      d.remove();
      return w;
    }
    const sc = getComputedStyle(document.querySelector('.scroll'));
    const tc = getComputedStyle(term);
    const sbScroll = sbWidth(
      `scrollbar-width:${sc.scrollbarWidth};scrollbar-color:${sc.scrollbarColor};`, ''
    );
    const sbTerm = sbWidth(
      `scrollbar-width:${tc.scrollbarWidth};scrollbar-color:${tc.scrollbarColor};`, ''
    );

    // ── 3. 各层 chrome：侧栏 + 卡片/终端/滚动区 的 padding+border ──
    const card = term.closest('.card');
    const side = document.querySelector('.side');
    const cc = getComputedStyle(card);
    const chrome =
      side.getBoundingClientRect().width + n(getComputedStyle(side).borderRightWidth) +
      n(sc.paddingLeft) + n(sc.paddingRight) +
      n(cc.borderLeftWidth) + n(cc.borderRightWidth) + n(cc.paddingLeft) + n(cc.paddingRight) +
      n(tc.borderLeftWidth) + n(tc.borderRightWidth) + n(tc.paddingLeft) + n(tc.paddingRight);

    const textW = Math.max(canvasW, domW);

    return {
      font: `${cs.fontSize} / ${cs.fontFamily}`,
      chars: LONGEST.length,
      px_per_char: +perChar.toFixed(4),
      textW_canvas: +canvasW.toFixed(2),
      textW_dom: +domW.toFixed(2),
      chrome: +chrome.toFixed(2),
      scrollbar_scroll: sbScroll,
      scrollbar_term: sbTerm,
      winBorder: 2,
      // 最坏情况：.scroll 与 .term 都有纵向滚动条
      windowWidth: +(chrome + textW + sbScroll + sbTerm + 2).toFixed(2),
      // 无 .term 滚动条（日志不满一屏）时
      windowWidth_noTermSB: +(chrome + textW + sbScroll + 2).toFixed(2),
    };
  }, LONGEST);

  console.log(JSON.stringify({
    ...r,
    '→ 建议窗口宽度（向上取整到整数）': Math.ceil(r.windowWidth),
    '→ 建议窗口宽度（对齐 5px 网格）': Math.ceil(r.windowWidth / 5) * 5,
  }, null, 2));

  await browser.close();
})();
