// 内联 SVG 图标注册表（WebUI 重构 C4）：stroke 1.5 / viewBox 24，去 emoji。
// 用法：icon('users') 返回 SVG 字符串；所有动态插值仍须过 esc()。

const defs = {
  gauge: '<path d="M12 14l3.5-3.5"/><circle cx="12" cy="14" r="1"/><path d="M20.5 17a9 9 0 1 0-17 0"/>',
  users: '<circle cx="9" cy="8" r="3.5"/><path d="M2.5 20a6.5 6.5 0 0 1 13 0"/><path d="M16 5.2a3.5 3.5 0 0 1 0 5.6"/><path d="M17.5 14.4a6.5 6.5 0 0 1 4 5.6"/>',
  layers: '<path d="m12 3 9 5-9 5-9-5 9-5Z"/><path d="m3 12.5 9 5 9-5"/><path d="m3 17 9 5 9-5"/>',
  key: '<circle cx="8" cy="15" r="4.5"/><path d="m11 12 9-9"/><path d="m17 6 2.5 2.5"/><path d="m14.5 8.5 2.5 2.5"/>',
  cpu: '<rect x="7" y="7" width="10" height="10" rx="2"/><rect x="10" y="10" width="4" height="4"/><path d="M12 3v3M12 18v3M3 12h3M18 12h3M5.5 5.5 7.5 7.5M16.5 16.5l2 2M18.5 5.5l-2 2M7.5 16.5l-2 2"/>',
  list: '<path d="M8 6h13M8 12h13M8 18h13"/><path d="M3.5 6h.01M3.5 12h.01M3.5 18h.01"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  copy: '<rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/>',
  check: '<path d="m4.5 12.5 5 5 10-11"/>',
  x: '<path d="M6 6l12 12M18 6 6 18"/>',
  alert: '<path d="M12 3 2.5 20h19L12 3Z"/><path d="M12 10v4"/><path d="M12 17.5v.01"/>',
  refresh: '<path d="M20 5v5h-5"/><path d="M4 19v-5h5"/><path d="M5.5 9a7 7 0 0 1 12.6-2L20 10"/><path d="M18.5 15a7 7 0 0 1-12.6 2L4 14"/>',
  trash: '<path d="M4 7h16"/><path d="M9 7V4h6v3"/><path d="M6 7l1 13h10l1-13"/><path d="M10 11v6M14 11v6"/>',
  star: '<path d="m12 3 2.7 5.8 6.3.7-4.7 4.2 1.3 6.1L12 16.7 6.4 19.8l1.3-6.1L3 9.5l6.3-.7L12 3Z"/>',
  shield: '<path d="M12 3 5 6v5c0 4.5 3 8.3 7 10 4-1.7 7-5.5 7-10V6l-7-3Z"/><path d="m9 11.5 2.5 2.5L15.5 10"/>',
  logout: '<path d="M15 4h4a1 1 0 0 1 1 1v14a1 1 0 0 1-1 1h-4"/><path d="M10 17l-5-5 5-5"/><path d="M5 12h11"/>',
  sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2.5v3M12 18.5v3M2.5 12h3M18.5 12h3M5 5l2 2M17 17l2 2M19 5l-2 2M7 17l-2 2"/>',
  moon: '<path d="M20 13.5A8 8 0 0 1 10.5 4 8 8 0 1 0 20 13.5Z"/>',
  contrast: '<circle cx="12" cy="12" r="9"/><path d="M12 3a9 9 0 0 0 0 18V3Z"/>',
  external: '<path d="M14 4h6v6"/><path d="M20 4 11 13"/><path d="M18 13.5V19a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5.5"/>',
  clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 3"/>',
  home: '<path d="m3 11 9-8 9 8"/><path d="M5 10v10h5v-6h4v6h5V10"/>',
  download: '<path d="M12 3v11"/><path d="m7 10 5 5 5-5"/><path d="M4 20h16"/>',
  upload: '<path d="M12 14V3"/><path d="m7 7 5-5 5 5"/><path d="M4 20h16"/>',
};

export function icon(name, cls = 'icon') {
  const d = defs[name] || defs.alert;
  return '<svg class="' + cls + '" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' + d + '</svg>';
}