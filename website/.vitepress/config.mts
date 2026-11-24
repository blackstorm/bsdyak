import { defineConfig } from 'vitepress'

export default defineConfig({
  title: 'Yak',
  description: 'Declarative, zero-magic FreeBSD Jail & ZFS deployment over SSH',
  lang: 'en-US',
  cleanUrls: true,
  lastUpdated: true,

  sitemap: {
    hostname: 'https://bsdyak.com',
  },

  head: [
    ['link', { rel: 'icon', type: 'image/svg+xml', href: '/logo.svg' }],
    ['meta', { property: 'og:site_name', content: 'Yak' }],
    ['meta', { property: 'og:type', content: 'website' }],
    ['meta', { property: 'og:url', content: 'https://bsdyak.com' }],
    ['meta', { name: 'theme-color', content: '#ab2b28' }],
  ],

  themeConfig: {
    logo: '/logo.svg',
    search: {
      provider: 'local',
    },
    socialLinks: [
      { icon: 'github', link: 'https://github.com/blackstorm/bsdyak' },
    ],
    nav: [
      { text: 'Guide', link: '/guide/what-is-yak' },
      { text: 'Recipes', link: '/guide/recipes' },
      { text: 'Reference', link: '/reference/cli' },
      { text: 'bsdyak.com', link: '/' },
    ],
    sidebar: [
      {
        text: 'Introduction',
        items: [
          { text: 'What is Yak?', link: '/guide/what-is-yak' },
          { text: 'Quickstart', link: '/guide/quickstart' },
          { text: 'Server Setup', link: '/guide/server-setup' },
        ],
      },
      {
        text: 'Core Concepts',
        items: [
          { text: 'Writing a Yakfile', link: '/guide/yakfile' },
          { text: 'Local & Server Builds', link: '/guide/builds' },
          { text: 'Networking & Expose', link: '/guide/networking' },
          { text: 'Operations & Composition', link: '/guide/operations' },
          { text: 'Real-World Recipes', link: '/guide/recipes' },
        ],
      },
      {
        text: 'Reference',
        items: [
          { text: 'CLI Commands', link: '/reference/cli' },
          { text: 'HCL Schema', link: '/reference/hcl-schema' },
          { text: 'ZFS & Host Layout', link: '/reference/zfs-layout' },
        ],
      },
    ],
    footer: {
      message: 'Released under the BSD-2-Clause License.',
      copyright: 'Copyright © 2026 Yak Contributors • https://bsdyak.com',
    },
  },
})
