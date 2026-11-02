import opentype from 'opentype.js';
import { Resvg } from '@resvg/resvg-js';
import fs from 'fs';
const F = 'node_modules/geist/dist/fonts/';
const load = f => opentype.parse(fs.readFileSync(F + f).buffer);
const sansB = load('geist-sans/Geist-SemiBold.ttf'), sansR = load('geist-sans/Geist-Regular.ttf');
const mono = load('geist-mono/GeistMono-Regular.ttf');
const OUT = process.argv[2]; fs.mkdirSync(OUT, { recursive: true });

const n = v => { const r = Math.round(v * 100) / 100; return Object.is(r, -0) ? '0' : String(r); };
function toD(path) {
  return path.commands.map(c => c.type === 'Z' ? 'Z'
    : c.type === 'M' || c.type === 'L' ? `${c.type}${n(c.x)} ${n(c.y)}`
    : c.type === 'Q' ? `Q${n(c.x1)} ${n(c.y1)} ${n(c.x)} ${n(c.y)}`
    : `C${n(c.x1)} ${n(c.y1)} ${n(c.x2)} ${n(c.y2)} ${n(c.x)} ${n(c.y)}`).join('');
}
function setText(font, text, x, y, size, trackEm = 0) {
  const gs = font.stringToGlyphs(text), sc = size / font.unitsPerEm, all = new opentype.Path();
  let cx = x;
  gs.forEach((g, i) => {
    all.extend(g.getPath(cx, y, size));
    cx += g.advanceWidth * sc + trackEm * size + (i < gs.length - 1 ? font.getKerningValue(g, gs[i + 1]) * sc : 0);
  });
  return { d: toD(all), bb: all.getBoundingBox() };
}
const W2 = (font, t, size, tr) => { const b = setText(font, t, 0, 0, size, tr).bb; return b.x2 - b.x1; };

// ---- wordmark in local coords: size 400, baseline y=0, left edge x=0
const SZ = 400;
const m0 = setText(sansB, 'kvz', 0, 0, SZ, -0.05);
const WM = m0; const OX = -m0.bb.x1;
const L = WM.bb.x1, R = WM.bb.x2, TOP = WM.bb.y1;                // TOP<0
const zb = setText(sansB, 'z', 0, 0, SZ).bb;
const zLeft = R - (zb.x2 - zb.x1), XT = zb.y1, BAR = 0.099 * SZ;
const rect = (a, b) => `M${a.toFixed(2)},${XT.toFixed(2)}H${b.toFixed(2)}V${(XT + BAR).toFixed(2)}H${a.toFixed(2)}Z`;
const BRIDGES = [rect(L + 158, L + 206), rect(zLeft - 24, zLeft + 10)];
const SUB = 'native macOS kubernetes', SUBSZ = 34, SUBY = 72;
const sw = W2(sansR, SUB, SUBSZ, 0.01), sb0 = setText(sansR, SUB, 0, 0, SUBSZ, 0.01).bb;
const SUBP = setText(sansR, SUB, R - sw - sb0.x1, SUBY, SUBSZ, 0.01).d;
const WMW = R - L;  // L may be non-zero; place() offsets by L

const RAINBOW = ['#ff3b5c', '#ff8a00', '#ffd400', '#2ee06d', '#2f8cff', '#a24bff'];
const THEMES = {
  dark: { bg: '#000', fg: '#fff', line: '#fff' },
  light: { bg: '#fafafa', fg: '#0a0a0a', line: '#000' },
};

// wordmark group; placed so local (0,0) → (x,y) at scale s
function wordmark({ x, y, s, fill, fg, sub = true, subOpacity = 0.6 }) {
  const paint = fill === 'rainbow' ? 'url(#rb)' : fg;
  return `<g transform="translate(${x.toFixed(2)} ${y.toFixed(2)}) scale(${s.toFixed(4)})">
    <defs><linearGradient id="rb" gradientUnits="userSpaceOnUse" x1="${L}" y1="0" x2="${R.toFixed(2)}" y2="0">${RAINBOW.map((c, i) => `<stop offset="${i / 5}" stop-color="${c}"/>`).join('')}</linearGradient></defs>
    <path d="${WM.d}${''}" fill="${paint}"/>${BRIDGES.map(b => `<path d="${b}" fill="${paint}"/>`).join('')}
    ${sub ? `<path d="${SUBP}" fill="${fg}" opacity="${subOpacity}"/>` : ''}
  </g>`;
}
// grid + construction guides + vignette
function backdrop(Wd, H, { x, y, s, line, guides = true, cell = 32, sub = true }) {
  const g = [];
  for (let i = 0; i <= Wd; i += cell) g.push(`M${i},0V${H}`);
  for (let i = 0; i <= H; i += cell) g.push(`M0,${i}H${Wd}`);
  const X = v => (x + v * s).toFixed(2), Y = v => (y + v * s).toFixed(2);
  const hs = [TOP, XT, 0].concat(sub ? [SUBY] : []), vs = [L, zLeft + 4.8, R];
  const gl = hs.map(v => `M0,${Y(v)}H${Wd}`).concat(vs.map(v => `M${X(v)},0V${H}`)).join('');
  const t = 9;
  const ticks = [TOP, 0].flatMap(yy => [L, R].map(xx => `M${+X(xx) - t},${Y(yy)}H${+X(xx) + t}M${X(xx)},${+Y(yy) - t}V${+Y(yy) + t}`)).join('');
  return `<defs>
    <radialGradient id="fade" cx="50%" cy="50%" r="${Wd > H ? 75 : 60}%"><stop offset="0" stop-color="#fff"/><stop offset=".55" stop-color="#fff" stop-opacity=".7"/><stop offset="1" stop-color="#fff" stop-opacity="0"/></radialGradient>
    <mask id="vig"><rect width="${Wd}" height="${H}" fill="url(#fade)"/></mask></defs>
  <g mask="url(#vig)" fill="none" stroke="${line}">
    <path d="${g.join('')}" stroke-width="1" opacity=".1"/>
    ${guides ? `<path d="${gl}" stroke-width="1" opacity=".38" stroke-dasharray="4 6"/><path d="${ticks}" stroke-width="1.5" opacity=".8"/>` : ''}
  </g>`;
}
function monoText(t, x, y, size, color, op, anchor = 'start') {
  const w = W2(mono, t, size, 0.02);
  const ax = anchor === 'end' ? x - w : anchor === 'middle' ? x - w / 2 : x;
  return `<path d="${setText(mono, t, ax, y, size, 0.02).d}" fill="${color}" opacity="${op}"/>`;
}

const files = [];
function emit(name, Wd, H, body, { bg, rx = 0, png = true } = {}) {
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${Wd} ${H}" width="${Wd}" height="${H}"><title>kvz</title>
  ${rx ? `<clipPath id="clip"><rect width="${Wd}" height="${H}" rx="${rx}"/></clipPath><g clip-path="url(#clip)">` : '<g>'}
  ${bg ? `<rect width="${Wd}" height="${H}" fill="${bg}"/>` : ''}${body}</g></svg>`;
  fs.writeFileSync(`${OUT}/${name}.svg`, svg);
  if (png) fs.writeFileSync(`${OUT}/${name}.png`, new Resvg(svg, { background: 'rgba(0,0,0,0)' }).render().asPng());
  files.push(name);
}
// place wordmark centred in a box: block height = TOP..(sub? SUBY+8 : 0)
function place(cx, cy, width, sub = true) {
  const s = width / WMW, hb = (sub ? SUBY + 8 : 0) - TOP;
  return { s, x: cx - WMW * s / 2 - L * s, y: cy - hb * s / 2 - TOP * s };
}
function card(name, Wd, H, theme, fill, { width, cy = H / 2, grid = true, guides = true, sub = true, extra = '', rx = 0, png } = {}) {
  const th = THEMES[theme], p = place(Wd / 2, cy, width, sub);
  emit(name, Wd, H, (grid ? backdrop(Wd, H, { ...p, line: th.line, guides, sub }) : '') + wordmark({ ...p, fill, fg: th.fg, sub }) + extra, { bg: th.bg, rx, png });
  return p;
}

// ---------- logos (1024 square)
card('logo-dark-rainbow', 1024, 1024, 'dark', 'rainbow', { width: 584 });
card('logo-dark-mono', 1024, 1024, 'dark', 'mono', { width: 584 });
card('logo-light-rainbow', 1024, 1024, 'light', 'rainbow', { width: 584 });
card('logo-light-mono', 1024, 1024, 'light', 'mono', { width: 584 });
card('logo-dark-rainbow-plain', 1024, 1024, 'dark', 'rainbow', { width: 584, grid: false });
// transparent wordmarks (for dropping on any background)
for (const [n, fill, fg] of [['wordmark-rainbow', 'rainbow', '#fff'], ['wordmark-white', 'mono', '#fff'], ['wordmark-black', 'mono', '#0a0a0a']]) {
  const pad = 24, s = 1, Wd = Math.ceil(WMW + pad * 2), H = Math.ceil(SUBY + 12 - TOP + pad * 2);
  emit(n, Wd, H, wordmark({ x: pad - L, y: pad - TOP, s, fill, fg }));
}
// ---------- app icon + favicon (no subtext)
card('icon-1024', 1024, 1024, 'dark', 'rainbow', { width: 720, sub: false, guides: false, rx: 228 });
card('icon-512', 512, 512, 'dark', 'rainbow', { width: 360, sub: false, guides: false, rx: 114 });
emit('favicon', 64, 64, wordmark({ ...place(32, 32, 52, false), fill: 'rainbow', fg: '#fff', sub: false }), { bg: '#000', rx: 14 });

// ---------- social / banners
const footer = (Wd, H, color) =>
  monoText('linux · macos · darwin runtimes', 64, H - 56, 20, color, 0.5) +
  monoText('single binary · vm-free control plane', Wd - 64, H - 56, 20, color, 0.5, 'end');
card('og-1200x630', 1200, 630, 'dark', 'rainbow', { width: 600, cy: 290, extra: footer(1200, 630, '#fff') });
card('og-light-1200x630', 1200, 630, 'light', 'rainbow', { width: 600, cy: 290, extra: footer(1200, 630, '#000') });
card('github-social-1280x640', 1280, 640, 'dark', 'rainbow', { width: 620, cy: 296, extra: footer(1280, 640, '#fff') });
card('x-header-1500x500', 1500, 500, 'dark', 'rainbow', { width: 560, cy: 250 });
card('linkedin-banner-1584x396', 1584, 396, 'dark', 'rainbow', { width: 440, cy: 198 });
card('readme-banner-1280x320', 1280, 320, 'dark', 'rainbow', { width: 420, cy: 160 });
card('readme-banner-light-1280x320', 1280, 320, 'light', 'rainbow', { width: 420, cy: 160 });
card('avatar-400', 400, 400, 'dark', 'rainbow', { width: 290, sub: false, guides: false });
console.log(files.join('\n'));
