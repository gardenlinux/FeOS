// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// FeChat's themes: "cyber", the default, and "gta", Vice City after
// Grand Theft Auto VI's artwork. The bolt in the logo switches between
// them. The theme is the attribute data-theme on <html>; styles.css holds
// the default's tokens, theme-gta.css the other's. The browser remembers
// the choice.

const STORAGE_KEY = 'fechat.theme'
const THEMES = ['cyber', 'gta']

// The words that change with the theme: the logo's name, the greeting of
// an empty chat, the composer's placeholder and the tab's title.
export const THEME_TEXT = {
  cyber: {
    name: 'FeChat',
    greeting: 'How can I help?',
    placeholder: 'Type a message …',
    title: 'FeChat',
  },
  gta: {
    name: 'vice chat',
    greeting: "What's the job?",
    placeholder: 'Lay out the plan …',
    title: 'Vice Chat',
  },
}

// The GTA theme's fonts load when it is first shown, so the default theme
// does not pay for them.
const FONTS = {
  gta: 'https://fonts.googleapis.com/css2?family=Changa+One&family=Martian+Mono:wdth,wght@87.5,400;87.5,600&family=Rubik:wght@400;500;700&display=swap',
}

export function currentTheme() {
  const t = document.documentElement.dataset.theme
  return THEMES.includes(t) ? t : 'cyber'
}

export function nextTheme(theme) {
  return THEMES[(THEMES.indexOf(theme) + 1) % THEMES.length]
}

function apply(theme) {
  document.documentElement.dataset.theme = theme
  document.title = THEME_TEXT[theme].title
  const href = FONTS[theme]
  if (href && !document.querySelector(`link[href="${href}"]`)) {
    const link = document.createElement('link')
    link.rel = 'stylesheet'
    link.href = href
    document.head.appendChild(link)
  }
  const icon = document.querySelector('link[rel="icon"]')
  if (icon) icon.href = theme === 'cyber' ? '/icon.svg' : `/icon-${theme}.svg`
}

export function setTheme(theme) {
  apply(theme)
  try {
    localStorage.setItem(STORAGE_KEY, theme)
  } catch {
    // private mode or full storage: the theme holds for this page
  }
}

// Before the first render: the remembered theme, else the default.
export function applyStoredTheme() {
  let theme = 'cyber'
  try {
    theme = localStorage.getItem(STORAGE_KEY) || theme
  } catch {
    // no storage: the default
  }
  apply(THEMES.includes(theme) ? theme : 'cyber')
}
