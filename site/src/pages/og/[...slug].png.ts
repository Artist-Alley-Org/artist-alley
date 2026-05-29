// Per-page OG image generator.
//
// Generates a 1200×630 PNG for every page in the `docs` collection at
// build time, served at /og/<slug>.png. SiteHead.astro injects the
// matching <meta property="og:image"> tag so Twitter / Slack / Discord /
// Mastodon previews look intentional out of the box.
//
// Powered by astro-og-canvas (skia-canvas WASM). Build-time only — no
// runtime cost on the live site.

import { OGImageRoute } from "astro-og-canvas";
import { getCollection } from "astro:content";

const docs = await getCollection("docs");

const pages = Object.fromEntries(
  docs.map((entry) => [
    entry.id,
    {
      data: {
        title: entry.data.title,
        description: entry.data.description ?? "",
      },
    },
  ]),
);

export const { getStaticPaths, GET } = await OGImageRoute({
  param: "slug",
  pages,
  getImageOptions: (_path, page) => ({
    title: page.data.title,
    description: page.data.description,

    // Layout / sizing.
    padding: 56,

    // Background gradient — matches the dark Starlight palette plus a hint of
    // the brand purple in the corner. Two RGB stops; astro-og-canvas blends.
    bgGradient: [
      [13, 17, 23], // top — near-black, matches --sl-color-bg
      [22, 27, 38], // bottom — slightly warmed
    ],

    // Brand accent on the left edge — the same purple used for view-transition
    // outlines and review-tool zone tints.
    border: {
      color: [124, 58, 237],
      width: 6,
      side: "inline-start",
    },

    // Typography.
    font: {
      title: {
        color: [240, 246, 252],
        weight: "ExtraBold",
        size: 72,
        families: ["Inter", "system-ui", "sans-serif"],
      },
      description: {
        color: [156, 163, 175],
        weight: "Normal",
        size: 30,
        families: ["Inter", "system-ui", "sans-serif"],
      },
    },

    // Logo in the upper-right via top padding.
    logo: {
      path: "./src/assets/logo.svg",
      size: [96],
    },
  }),
});
